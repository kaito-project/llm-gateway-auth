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
// This file tests the Istio ingress gateway integration:
//   - Deploys an echo backend, Gateway, and VirtualService
//   - Sends HTTP requests through the Istio ingress gateway
//   - Verifies that the ext_authz AuthorizationPolicy enforces API key auth
//
// Prerequisites:
//
//	./hack/setup-test-env.sh   # kind + Istio with ingress gateway + Helm
//
// Run with:
//
//	make e2e-test
package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	v1alpha1 "github.com/kaito-project/llm-gateway-auth/api/v1alpha1"
)

func TestIstioIngressAuthz(t *testing.T) {
	// ── Setup: namespace, API key, echo backend, Istio routing ──────────
	testNS := createNamespace(t, "e2e-istio")

	// Create APIKey and wait for it to become Active with a Secret.
	_ = createAPIKey(t, testNS, "720h")
	waitForAPIKeyPhase(t, testNS, apiKeyName, v1alpha1.PhaseActive, defaultTimeout)
	sec := waitForSecret(t, testNS, apiKeySecret, defaultTimeout)
	rawKey := string(sec.Data["apiKey"])
	assert.NotEmpty(t, rawKey)
	t.Logf("API key ready: %s... (len=%d)", rawKey[:6], len(rawKey))

	// Deploy echo server as the backend.
	deployEchoServer(t, testNS)

	// Create Gateway + VirtualService routing host → echo service.
	host := namespaceHost(testNS)
	applyIstioResources(t, testNS, host)

	// Port-forward to the Istio ingress gateway.
	gwAddr := portForwardIngressGateway(t)
	t.Logf("Ingress gateway forwarded to %s", gwAddr)

	// Allow time for the AuthorizationPolicy, Gateway, and VirtualService
	// to propagate through Envoy configuration and for the ext_authz index
	// to sync the new key.
	time.Sleep(indexSyncWait + 10*time.Second)

	// ── Sub-tests ───────────────────────────────────────────────────────

	t.Run("allow_valid_key", func(t *testing.T) {
		// The route may take a moment to propagate; poll until we get 200.
		var code int
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			code = httpGet(t, gwAddr, host, "Bearer "+rawKey, "/")
			if code == http.StatusOK {
				break
			}
			time.Sleep(time.Second)
		}
		assert.Equal(t, http.StatusOK, code,
			"valid key + correct host should be allowed through to echo backend")
	})

	t.Run("deny_missing_auth_header", func(t *testing.T) {
		code := httpGet(t, gwAddr, host, "", "/")
		// ext_authz returns 401 Unauthorized for missing/invalid credentials.
		assert.Equal(t, http.StatusUnauthorized, code,
			"missing Authorization header should be denied by ext_authz")
	})

	t.Run("deny_invalid_key", func(t *testing.T) {
		code := httpGet(t, gwAddr, host, "Bearer sk-totally-bogus-key", "/")
		assert.Equal(t, http.StatusUnauthorized, code,
			"invalid key should be denied by ext_authz")
	})

	t.Run("deny_wrong_host", func(t *testing.T) {
		code := httpGet(t, gwAddr, "wrong-namespace."+gwDomain, "Bearer "+rawKey, "/")
		// Wrong host may result in 401 (authz denial) or 404 (no route).
		// Either way it must NOT be 200.
		assert.NotEqual(t, http.StatusOK, code,
			"valid key + wrong host should not reach the backend")
	})
}
