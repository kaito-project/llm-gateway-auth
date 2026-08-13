// Copyright (c) KAITO authors.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build e2e

// Package e2e contains end-to-end tests that run against a real kind cluster.
//
// This file tests the full API key lifecycle:
//   - Create APIKey CR → operator creates Secret with the raw key
//   - IndexSyncer populates the in-memory index from the Secret
//   - gRPC ext_authz Check: allow with correct key+host, deny otherwise
//
// Prerequisites:
//
//	make setup-test-env                          # kind + Istio (or kind-create for no Istio)
//	make kind-load-operator kind-load-authz      # build + load images
//	helm install llm-gateway-apikey chart/llm-gateway-apikey \
//	  --namespace llm-gateway --create-namespace \
//	  --set operator.image.repository=localhost/apikey-operator \
//	  --set operator.image.tag=latest \
//	  --set authz.image.repository=localhost/apikey-authz \
//	  --set authz.image.tag=latest \
//	  --set global.imagePullPolicy=Never \
//	  --set istio.enabled=false
//
// Run with:
//
//	make e2e-test
package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/kaito-project/llm-gateway-auth/api/v1alpha1"
)

func TestAPIKeyLifecycle(t *testing.T) {
	testNS := createNamespace(t, "e2e-apikey")
	_ = createAPIKey(t, testNS, "720h")

	// Wait for the operator to create the Secret.
	sec := waitForSecret(t, testNS, apiKeySecret, defaultTimeout)
	rawKey := string(sec.Data["apiKey"])
	assert.NotEmpty(t, rawKey, "Secret should contain an apiKey")
	assert.True(t, strings.HasPrefix(rawKey, defaultPrefix), "key should have the configured prefix")
	t.Logf("API key created: %s... (len=%d)", rawKey[:6], len(rawKey))

	// Port-forward to the authz gRPC service.
	authzClient := setupAuthzClient(t)
	time.Sleep(indexSyncWait)
	host := namespaceHost(testNS)

	// Valid key + correct namespace host → ALLOW.
	t.Run("allow_valid_key_correct_host", func(t *testing.T) {
		resp := assertAllowed(t, authzClient, makeCheckRequest(host, "Bearer "+rawKey, "/v1/chat/completions"))

		// Verify tenant headers are injected.
		okResp := resp.GetOkResponse()
		assert.NotNil(t, okResp, "expected OkResponse")
		headerMap := map[string]string{}
		for _, h := range okResp.GetHeaders() {
			headerMap[h.GetHeader().GetKey()] = h.GetHeader().GetValue()
		}
		assert.Equal(t, testNS, headerMap["x-tenant-namespace"])
		assert.NotEmpty(t, headerMap["x-key-id"])
		t.Logf("x-tenant-namespace=%s x-key-id=%s", headerMap["x-tenant-namespace"], headerMap["x-key-id"])
	})

	// Valid key + wrong namespace host → DENY.
	t.Run("deny_valid_key_wrong_host", func(t *testing.T) {
		assertDenied(t, authzClient, makeCheckRequest("wrong-namespace."+gwDomain, "Bearer "+rawKey, "/v1/chat/completions"))
	})

	// Missing Authorization header → DENY.
	t.Run("deny_missing_auth_header", func(t *testing.T) {
		assertDenied(t, authzClient, makeCheckRequest(host, "", "/v1/chat/completions"))
	})

	// Invalid key → DENY.
	t.Run("deny_invalid_key", func(t *testing.T) {
		assertDenied(t, authzClient, makeCheckRequest(host, "Bearer sk-totally-bogus-key", "/v1/chat/completions"))
	})

	// No host subdomain → DENY (cannot resolve namespace).
	t.Run("deny_no_host_subdomain", func(t *testing.T) {
		assertDenied(t, authzClient, makeCheckRequest("localhost", "Bearer "+rawKey, "/v1/chat/completions"))
	})
}

func TestAPIKeyRotation(t *testing.T) {
	testNS := createNamespace(t, "e2e-rotate")
	apiKey := createAPIKey(t, testNS, "720h")

	// Wait for the initial key to become Active.
	waitForAPIKeyPhase(t, testNS, apiKeyName, v1alpha1.PhaseActive, defaultTimeout)
	waitForSecret(t, testNS, apiKeySecret, defaultTimeout)

	// Trigger rotation by setting rotateAt to the past with a short grace period.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(apiKey), apiKey); err != nil {
			return err
		}
		past := metav1.NewTime(time.Now().Add(-1 * time.Second))
		apiKey.Spec.Rotation = &v1alpha1.RotationSpec{
			RotateAt:    &past,
			GracePeriod: "1m",
		}
		return k8sClient.Update(ctx, apiKey)
	})
	require.NoError(t, err)

	// Wait for rotation to complete (status.Secondary is set).
	rotatedKey := waitForAPIKeyRotation(t, testNS, apiKeyName, defaultTimeout)
	assert.NotNil(t, rotatedKey.Status.Primary)
	assert.NotNil(t, rotatedKey.Status.Secondary)
	assert.NotEqual(t, rotatedKey.Status.Primary.KeyID, rotatedKey.Status.Secondary.KeyID,
		"primary and secondary keyIDs should differ after rotation")
	t.Logf("Rotation complete: newKeyID=%s secondaryKeyID=%s",
		rotatedKey.Status.Primary.KeyID, rotatedKey.Status.Secondary.KeyID)

	// Fetch the rotated Secret — it should have both keys.
	var rotatedSec corev1.Secret
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: testNS, Name: apiKeySecret}, &rotatedSec))
	newKey := string(rotatedSec.Data["apiKey"])
	secondaryKey := string(rotatedSec.Data["secondaryApiKey"])
	assert.NotEmpty(t, newKey)
	assert.NotEmpty(t, secondaryKey)
	assert.NotEqual(t, newKey, secondaryKey, "new and secondary keys should differ")
	t.Logf("New key: %s... Secondary key: %s...", newKey[:6], secondaryKey[:6])

	// Port-forward to authz and verify both keys work during grace period.
	authzClient := setupAuthzClient(t)
	time.Sleep(indexSyncWait)
	host := namespaceHost(testNS)

	t.Run("allow_new_key", func(t *testing.T) {
		assertAllowed(t, authzClient, makeCheckRequest(host, "Bearer "+newKey, "/v1/chat/completions"))
	})

	t.Run("allow_previous_key_during_grace_period", func(t *testing.T) {
		assertAllowed(t, authzClient, makeCheckRequest(host, "Bearer "+secondaryKey, "/v1/chat/completions"))
	})

	t.Run("deny_bogus_key_after_rotation", func(t *testing.T) {
		assertDenied(t, authzClient, makeCheckRequest(host, "Bearer sk-totally-bogus", "/v1/chat/completions"))
	})
}

func TestAPIKeyExpiry(t *testing.T) {
	testNS := createNamespace(t, "e2e-expiry")
	_ = createAPIKey(t, testNS, "3s")

	// Wait for the key to become Active first.
	waitForAPIKeyPhase(t, testNS, apiKeyName, v1alpha1.PhaseActive, defaultTimeout)

	// Capture the key.
	sec := waitForSecret(t, testNS, apiKeySecret, defaultTimeout)
	rawKey := string(sec.Data["apiKey"])
	assert.NotEmpty(t, rawKey)
	t.Logf("Key created: %s... (len=%d, expiresIn=3s)", rawKey[:6], len(rawKey))

	// Port-forward and verify the key works while active.
	authzClient := setupAuthzClient(t)
	time.Sleep(indexSyncWait)
	host := namespaceHost(testNS)

	t.Run("allow_before_expiry", func(t *testing.T) {
		assertAllowed(t, authzClient, makeCheckRequest(host, "Bearer "+rawKey, "/v1/chat/completions"))
	})

	// Wait for the key to expire.
	expiredKey := waitForAPIKeyPhase(t, testNS, apiKeyName, v1alpha1.PhaseExpired, defaultTimeout)
	assert.NotNil(t, expiredKey.Status.Primary)
	assert.NotNil(t, expiredKey.Status.Primary.ExpiresAt)
	t.Logf("Key expired at %s", expiredKey.Status.Primary.ExpiresAt.Format(time.RFC3339))

	// Verify the status condition.
	readyCond := findCondition(expiredKey.Status.Conditions, "Ready")
	assert.NotNil(t, readyCond, "expected Ready condition")
	if readyCond != nil {
		assert.Equal(t, metav1.ConditionFalse, readyCond.Status)
		assert.Equal(t, "Expired", readyCond.Reason)
		t.Logf("Ready condition: status=%s reason=%s message=%s",
			readyCond.Status, readyCond.Reason, readyCond.Message)
	}
}
