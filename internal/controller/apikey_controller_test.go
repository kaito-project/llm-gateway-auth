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

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/kaito-project/llm-gateway-auth/api/v1alpha1"
	"github.com/kaito-project/llm-gateway-auth/internal/keyutil"
)

func setupScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	clientgoscheme.AddToScheme(s)
	v1alpha1.AddToScheme(s)
	return s
}

type capturedEvent struct {
	EventType string
	Reason    string
	Action    string
	Note      string
}

type capturedEventRecorder struct {
	Events []capturedEvent
}

func (r *capturedEventRecorder) Eventf(_ runtime.Object, _ runtime.Object, eventType, reason, action, note string, args ...any) {
	r.Events = append(r.Events, capturedEvent{
		EventType: eventType,
		Reason:    reason,
		Action:    action,
		Note:      fmt.Sprintf(note, args...),
	})
}

func requireCapturedEvent(t *testing.T, recorder *capturedEventRecorder, expected capturedEvent) {
	t.Helper()
	require.Equal(t, []capturedEvent{expected}, recorder.Events)
}

func requireEvent(t *testing.T, recorder *events.FakeRecorder, expected string) {
	t.Helper()
	select {
	case event := <-recorder.Events:
		require.Equal(t, expected, event)
	default:
		t.Fatal("expected Kubernetes Event, got none")
	}
}

func requireNoEvent(t *testing.T, recorder *events.FakeRecorder) {
	t.Helper()
	select {
	case event := <-recorder.Events:
		t.Fatalf("expected no Kubernetes Event, got %q", event)
	default:
	}
}

func findReadyCondition(t *testing.T, apiKey *v1alpha1.APIKey) *metav1.Condition {
	t.Helper()
	for i := range apiKey.Status.Conditions {
		if apiKey.Status.Conditions[i].Type == "Ready" {
			return &apiKey.Status.Conditions[i]
		}
	}
	t.Fatal("expected Ready condition")
	return nil
}

func TestReconcile_InvalidName(t *testing.T) {
	s := setupScheme()
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "not-default",
			Namespace: "team-alpha",
		},
		Spec: v1alpha1.APIKeySpec{
			Prefix: "sk-",
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey).WithStatusSubresource(apiKey).Build()
	r := &APIKeyReconciler{Client: c}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "not-default"},
	})
	require.NoError(t, err)
}

func TestReconcile_NotFound(t *testing.T) {
	s := setupScheme()
	c := fake.NewClientBuilder().WithScheme(s).Build()
	r := &APIKeyReconciler{Client: c}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "default"},
	})
	require.NoError(t, err)
}

func TestReconcile_GeneratesSecret(t *testing.T) {
	s := setupScheme()
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default",
			Namespace: "team-alpha",
		},
		Spec: v1alpha1.APIKeySpec{
			Prefix: "sk-",
		},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey).WithStatusSubresource(apiKey).Build()
	recorder := events.NewFakeRecorder(10)
	r := &APIKeyReconciler{Client: c, Recorder: recorder, Clock: clocktesting.NewFakeClock(now)}

	// First reconcile: should create Secret and set status.secretRef
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "default"},
	})
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter, "expected requeue after Secret creation")
	requireEvent(t, recorder, `Normal Created Stored newly generated API key material in Secret "llm-api-key"`)

	// Verify Secret was created
	var secret corev1.Secret
	err = c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"}, &secret)
	require.NoError(t, err, "expected Secret to be created")

	rawKey := string(secret.Data[keyutil.SecretKey])
	assert.NotEmpty(t, rawKey, "expected non-empty API key in Secret")
	assert.NotEmpty(t, secret.Data[keyutil.SecondarySecretKey])
	primaryExpiresAt, err := keyutil.ParsePrimaryExpiry(secret.Annotations)
	require.NoError(t, err)
	assert.True(t, primaryExpiresAt.IsZero(), "non-expiring primary must use explicit never metadata")
	secondaryExpiresAt, err := keyutil.ParseSecondaryExpiry(secret.Annotations)
	require.NoError(t, err)
	assert.Equal(t, now.Add(v1alpha1.DefaultGracePeriod), secondaryExpiresAt)
	assert.Equal(t, keyutil.HashKey(rawKey), secret.Annotations[keyutil.PrimaryHashAnnotation])
	assert.Equal(t, keyutil.HashKey(string(secret.Data[keyutil.SecondarySecretKey])), secret.Annotations[keyutil.SecondaryHashAnnotation])

	// Verify status.secretRef was set
	var updated v1alpha1.APIKey
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated)
	require.NotNil(t, updated.Status.SecretRef, "expected status.secretRef to be set")
	assert.Equal(t, "llm-api-key", updated.Status.SecretRef.Name)

	// Second reconcile: should read Secret and update status
	_, err = r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "default"},
	})
	require.NoError(t, err, "unexpected error on second reconcile")
	requireNoEvent(t, recorder)

	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated)
	assert.Equal(t, v1alpha1.PhaseActive, updated.Status.Phase)
	require.NotNil(t, updated.Status.Primary, "expected primary to be set")
	assert.NotEmpty(t, updated.Status.Primary.KeyID, "expected primary.keyID to be set")
}

func TestReconcile_SecretCreateFailureEmitsEvent(t *testing.T) {
	s := setupScheme()
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "team-alpha"},
		Spec:       v1alpha1.APIKeySpec{Prefix: "sk-"},
	}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(apiKey).
		WithStatusSubresource(apiKey).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				return assert.AnError
			},
		}).
		Build()
	recorder := &capturedEventRecorder{}
	r := &APIKeyReconciler{Client: c, Recorder: recorder}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "default"},
	})
	require.ErrorIs(t, err, assert.AnError)
	requireCapturedEvent(t, recorder, capturedEvent{
		EventType: corev1.EventTypeWarning,
		Reason:    eventReasonSecretWriteFailed,
		Action:    eventActionCreateSecret,
		Note:      `Failed to create Secret "llm-api-key"`,
	})
}

func TestReconcile_ExistingSecretInStatus(t *testing.T) {
	s := setupScheme()
	rawKey := "sk-llmgw-Jx3kLmNpQrStUvWxYzAbCdEfGh0123456789AB"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llm-api-key",
			Namespace: "team-alpha",
		},
		Data: map[string][]byte{
			"apiKey": []byte(rawKey),
		},
	}
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default",
			Namespace: "team-alpha",
		},
		Spec: v1alpha1.APIKeySpec{
			Prefix: "sk-",
		},
		Status: v1alpha1.APIKeyStatus{
			SecretRef: &v1alpha1.SecretReference{
				Name: "llm-api-key",
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey, secret).WithStatusSubresource(apiKey).Build()
	r := &APIKeyReconciler{Client: c}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "default"},
	})
	require.NoError(t, err)

	var updated v1alpha1.APIKey
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated)
	assert.Equal(t, v1alpha1.PhaseActive, updated.Status.Phase)
}

func TestReconcile_ExpiredKey(t *testing.T) {
	s := setupScheme()
	createdAt := time.Date(2026, time.October, 8, 11, 0, 0, 0, time.UTC)
	expiresAt := createdAt.Add(time.Hour)
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "default",
			Namespace:         "team-alpha",
			UID:               types.UID("expiry-uid"),
			CreationTimestamp: metav1.NewTime(createdAt),
		},
		Spec: v1alpha1.APIKeySpec{Prefix: "llmgw", ExpiresIn: "1h"},
		Status: v1alpha1.APIKeyStatus{
			Phase:     v1alpha1.PhaseActive,
			Primary:   &v1alpha1.KeyStatus{KeyID: keyutil.KeyIDFromHash(keyutil.HashKey("sk-expired")), ExpiresAt: &metav1.Time{Time: expiresAt}},
			Secondary: &v1alpha1.KeyStatus{KeyID: "secondary-id", ExpiresAt: &metav1.Time{Time: expiresAt}},
			SecretRef: &v1alpha1.SecretReference{Name: "custom-key-secret"},
		},
	}
	ownedSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "custom-key-secret",
			Namespace:       apiKey.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(apiKey, v1alpha1.GroupVersion.WithKind("APIKey"))},
		},
		Data: map[string][]byte{keyutil.SecretKey: []byte("sk-expired")},
	}
	unrelatedSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: keyutil.SecretName, Namespace: apiKey.Namespace},
		Data:       map[string][]byte{keyutil.SecretKey: []byte("do-not-delete")},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey, ownedSecret, unrelatedSecret).WithStatusSubresource(apiKey).Build()
	recorder := events.NewFakeRecorder(10)
	r := &APIKeyReconciler{Client: c, Recorder: recorder, Clock: clocktesting.NewFakeClock(expiresAt)}

	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "default"}}
	_, err := r.Reconcile(context.Background(), request)
	require.NoError(t, err)

	var deleted corev1.Secret
	err = c.Get(context.Background(), client.ObjectKeyFromObject(ownedSecret), &deleted)
	assert.True(t, apierrors.IsNotFound(err), "owned Secret must be deleted at the exact expiry boundary")
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(unrelatedSecret), &deleted))
	assert.Equal(t, "do-not-delete", string(deleted.Data[keyutil.SecretKey]))

	var updated v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), request.NamespacedName, &updated))
	assert.Equal(t, v1alpha1.PhaseExpired, updated.Status.Phase)
	require.NotNil(t, updated.Status.Primary)
	assert.Equal(t, keyutil.KeyIDFromHash(keyutil.HashKey("sk-expired")), updated.Status.Primary.KeyID)
	assert.True(t, updated.Status.Primary.ExpiresAt.Time.Equal(expiresAt))
	assert.Nil(t, updated.Status.Secondary)
	assert.Nil(t, updated.Status.SecretRef)
	requireEvent(t, recorder, "Warning Expired "+findReadyCondition(t, &updated).Message)

	_, err = r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	requireNoEvent(t, recorder)
}

func TestReconcile_ExpiryDeleteRetriesBeforeStatus(t *testing.T) {
	s := setupScheme()
	expiresAt := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "default",
			Namespace:         "team-alpha",
			UID:               types.UID("expiry-retry-uid"),
			CreationTimestamp: metav1.NewTime(expiresAt.Add(-time.Hour)),
			Finalizers:        []string{finalizerName},
		},
		Spec: v1alpha1.APIKeySpec{ExpiresIn: "1h"},
		Status: v1alpha1.APIKeyStatus{
			Phase:     v1alpha1.PhaseActive,
			Primary:   &v1alpha1.KeyStatus{KeyID: "primary-id", ExpiresAt: &metav1.Time{Time: expiresAt}},
			SecretRef: &v1alpha1.SecretReference{Name: keyutil.SecretName},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            keyutil.SecretName,
			Namespace:       apiKey.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(apiKey, v1alpha1.GroupVersion.WithKind("APIKey"))},
		},
	}
	deleteAttempts := 0
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey, secret).WithStatusSubresource(apiKey).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, delegate client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deleteAttempts++
				if deleteAttempts == 1 {
					return assert.AnError
				}
				return delegate.Delete(ctx, obj, opts...)
			},
		}).Build()
	r := &APIKeyReconciler{Client: c, Clock: clocktesting.NewFakeClock(expiresAt)}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(apiKey)}

	_, err := r.Reconcile(context.Background(), request)
	require.ErrorIs(t, err, assert.AnError)
	var afterFailure v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), request.NamespacedName, &afterFailure))
	assert.Equal(t, v1alpha1.PhaseActive, afterFailure.Status.Phase, "status must not advance before Secret deletion")

	_, err = r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	var deleted corev1.Secret
	assert.True(t, apierrors.IsNotFound(c.Get(context.Background(), client.ObjectKeyFromObject(secret), &deleted)))
	var expired v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), request.NamespacedName, &expired))
	assert.Equal(t, v1alpha1.PhaseExpired, expired.Status.Phase)
	assert.Equal(t, 2, deleteAttempts)
}

func TestReconcile_ExpiryUsesAuthoritativeReaderWhenCacheIsStale(t *testing.T) {
	s := setupScheme()
	expiresAt := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	rawKey := "sk-expired-with-stale-cache"
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "default",
			Namespace:         "team-alpha",
			UID:               types.UID("stale-cache-api-key"),
			CreationTimestamp: metav1.NewTime(expiresAt.Add(-time.Hour)),
			Finalizers:        []string{finalizerName},
		},
		Spec: v1alpha1.APIKeySpec{ExpiresIn: "1h"},
		Status: v1alpha1.APIKeyStatus{
			Phase:     v1alpha1.PhaseActive,
			Primary:   &v1alpha1.KeyStatus{KeyID: keyutil.KeyIDFromHash(keyutil.HashKey(rawKey)), ExpiresAt: &metav1.Time{Time: expiresAt}},
			SecretRef: &v1alpha1.SecretReference{Name: keyutil.SecretName},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keyutil.SecretName,
			Namespace: apiKey.Namespace,
			UID:       types.UID("stale-cache-secret"),
			Annotations: map[string]string{
				keyutil.PrimaryExpiryAnnotation: expiresAt.Format(time.RFC3339Nano),
				keyutil.PrimaryHashAnnotation:   keyutil.HashKey(rawKey),
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(apiKey, v1alpha1.GroupVersion.WithKind("APIKey"))},
		},
		Data: map[string][]byte{keyutil.SecretKey: []byte(rawKey)},
	}

	authoritative := fake.NewClientBuilder().WithScheme(s).WithObjects(secret.DeepCopy()).Build()
	cached := fake.NewClientBuilder().WithScheme(s).
		WithObjects(apiKey.DeepCopy(), secret.DeepCopy()).
		WithStatusSubresource(apiKey).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, _ client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				return authoritative.Delete(ctx, obj, opts...)
			},
		}).Build()
	r := &APIKeyReconciler{
		Client:    cached,
		APIReader: authoritative,
		Clock:     clocktesting.NewFakeClock(expiresAt),
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(apiKey)})
	require.NoError(t, err)

	var authoritativeSecret corev1.Secret
	assert.True(t, apierrors.IsNotFound(authoritative.Get(context.Background(), client.ObjectKeyFromObject(secret), &authoritativeSecret)))
	var staleSecret corev1.Secret
	require.NoError(t, cached.Get(context.Background(), client.ObjectKeyFromObject(secret), &staleSecret), "test cache must remain stale after API deletion")
	var expired v1alpha1.APIKey
	require.NoError(t, cached.Get(context.Background(), client.ObjectKeyFromObject(apiKey), &expired))
	assert.Equal(t, v1alpha1.PhaseExpired, expired.Status.Phase)
	assert.Nil(t, expired.Status.SecretRef)
}

func TestReconcile_ExpiryUIDPreconditionProtectsConcurrentReplacement(t *testing.T) {
	s := setupScheme()
	expiresAt := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	rawKey := "sk-expired-before-delete-race"
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "team-alpha", UID: types.UID("delete-race-api-key"), Finalizers: []string{finalizerName}},
		Status: v1alpha1.APIKeyStatus{
			Phase:     v1alpha1.PhaseActive,
			Primary:   &v1alpha1.KeyStatus{KeyID: keyutil.KeyIDFromHash(keyutil.HashKey(rawKey)), ExpiresAt: &metav1.Time{Time: expiresAt}},
			SecretRef: &v1alpha1.SecretReference{Name: keyutil.SecretName},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keyutil.SecretName,
			Namespace: apiKey.Namespace,
			UID:       types.UID("delete-race-original"),
			Annotations: map[string]string{
				keyutil.PrimaryExpiryAnnotation: expiresAt.Format(time.RFC3339Nano),
				keyutil.PrimaryHashAnnotation:   keyutil.HashKey(rawKey),
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(apiKey, v1alpha1.GroupVersion.WithKind("APIKey"))},
		},
		Data: map[string][]byte{keyutil.SecretKey: []byte(rawKey)},
	}

	replaced := false
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey, secret).WithStatusSubresource(apiKey).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, delegate client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if !replaced {
					replaced = true
					require.NoError(t, delegate.Delete(ctx, obj))
					replacementRaw := "sk-concurrent-replacement"
					replacement := secret.DeepCopy()
					replacement.UID = types.UID("delete-race-replacement")
					replacement.ResourceVersion = ""
					replacement.Data[keyutil.SecretKey] = []byte(replacementRaw)
					replacement.Annotations[keyutil.PrimaryExpiryAnnotation] = expiresAt.Add(time.Hour).Format(time.RFC3339Nano)
					replacement.Annotations[keyutil.PrimaryHashAnnotation] = keyutil.HashKey(replacementRaw)
					require.NoError(t, delegate.Create(ctx, replacement))

					deleteOptions := &client.DeleteOptions{}
					for _, option := range opts {
						option.ApplyToDelete(deleteOptions)
					}
					require.NotNil(t, deleteOptions.Preconditions)
					require.NotNil(t, deleteOptions.Preconditions.UID)
					assert.Equal(t, secret.UID, *deleteOptions.Preconditions.UID)
					return fmt.Errorf("API server rejected stale UID precondition")
				}
				return delegate.Delete(ctx, obj, opts...)
			},
		}).Build()
	r := &APIKeyReconciler{Client: c, APIReader: c, Clock: clocktesting.NewFakeClock(expiresAt)}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(apiKey)})
	require.Error(t, err, "the UID precondition must reject deleting a replacement with the old object")
	var replacement corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), &replacement))
	assert.Equal(t, types.UID("delete-race-replacement"), replacement.UID)
	assert.Equal(t, "sk-concurrent-replacement", string(replacement.Data[keyutil.SecretKey]))
	var stillActive v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(apiKey), &stillActive))
	assert.NotEqual(t, v1alpha1.PhaseExpired, stillActive.Status.Phase)
}

func TestReconcile_ExpiryDeletesSameNameOwnedReplacement(t *testing.T) {
	s := setupScheme()
	expiresAt := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	rawKey := "sk-expired-before-replacement"
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "team-alpha", UID: types.UID("replacement-api-key"), Finalizers: []string{finalizerName}},
		Status: v1alpha1.APIKeyStatus{
			Phase:     v1alpha1.PhaseActive,
			Primary:   &v1alpha1.KeyStatus{KeyID: keyutil.KeyIDFromHash(keyutil.HashKey(rawKey)), ExpiresAt: &metav1.Time{Time: expiresAt}},
			SecretRef: &v1alpha1.SecretReference{Name: keyutil.SecretName},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keyutil.SecretName,
			Namespace: apiKey.Namespace,
			UID:       types.UID("original-secret"),
			Annotations: map[string]string{
				keyutil.PrimaryExpiryAnnotation: expiresAt.Format(time.RFC3339Nano),
				keyutil.PrimaryHashAnnotation:   keyutil.HashKey(rawKey),
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(apiKey, v1alpha1.GroupVersion.WithKind("APIKey"))},
		},
		Data: map[string][]byte{keyutil.SecretKey: []byte(rawKey)},
	}

	deleteCalls := 0
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey, secret).WithStatusSubresource(apiKey).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, delegate client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deleteCalls++
				if err := delegate.Delete(ctx, obj, opts...); err != nil {
					return err
				}
				if deleteCalls == 1 {
					replacementRaw := "sk-future-owned-replacement"
					replacement := secret.DeepCopy()
					replacement.UID = types.UID("replacement-secret")
					replacement.ResourceVersion = ""
					replacement.Data[keyutil.SecretKey] = []byte(replacementRaw)
					replacement.Annotations[keyutil.PrimaryExpiryAnnotation] = expiresAt.Add(time.Hour).Format(time.RFC3339Nano)
					replacement.Annotations[keyutil.PrimaryHashAnnotation] = keyutil.HashKey(replacementRaw)
					return delegate.Create(ctx, replacement)
				}
				return nil
			},
		}).Build()
	r := &APIKeyReconciler{Client: c, APIReader: c, Clock: clocktesting.NewFakeClock(expiresAt)}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(apiKey)})
	require.NoError(t, err)
	assert.Equal(t, 2, deleteCalls)
	var remaining corev1.Secret
	assert.True(t, apierrors.IsNotFound(c.Get(context.Background(), client.ObjectKeyFromObject(secret), &remaining)))
	var expired v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(apiKey), &expired))
	assert.Equal(t, v1alpha1.PhaseExpired, expired.Status.Phase)
}

func TestReconcile_ExpiryWaitsForFinalizedSecretDeletion(t *testing.T) {
	s := setupScheme()
	expiresAt := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "default",
			Namespace:         "team-alpha",
			UID:               types.UID("expiry-finalizer-uid"),
			CreationTimestamp: metav1.NewTime(expiresAt.Add(-time.Hour)),
		},
		Spec: v1alpha1.APIKeySpec{ExpiresIn: "1h"},
		Status: v1alpha1.APIKeyStatus{
			Phase:     v1alpha1.PhaseActive,
			Primary:   &v1alpha1.KeyStatus{KeyID: "primary-id", ExpiresAt: &metav1.Time{Time: expiresAt}},
			SecretRef: &v1alpha1.SecretReference{Name: keyutil.SecretName},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:       keyutil.SecretName,
			Namespace:  apiKey.Namespace,
			UID:        types.UID("finalized-secret-uid"),
			Finalizers: []string{"example.test/retain-secret"},
			Annotations: map[string]string{
				keyutil.PrimaryExpiryAnnotation: expiresAt.Format(time.RFC3339Nano),
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(apiKey, v1alpha1.GroupVersion.WithKind("APIKey"))},
		},
		Data: map[string][]byte{keyutil.SecretKey: []byte("sk-llmgw-finalized-expired-key")},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey, secret).WithStatusSubresource(apiKey).Build()
	r := &APIKeyReconciler{Client: c, Clock: clocktesting.NewFakeClock(expiresAt)}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(apiKey)}

	_, err := r.Reconcile(context.Background(), request)
	require.ErrorContains(t, err, "Secret still exists")

	var retained corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), &retained))
	assert.False(t, retained.DeletionTimestamp.IsZero())
	assert.Equal(t, secret.Data, retained.Data)

	var active v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), request.NamespacedName, &active))
	assert.Equal(t, v1alpha1.PhaseActive, active.Status.Phase)
	assert.NotNil(t, active.Status.SecretRef)
}

func TestReconcile_SecretDeletedRegenerates(t *testing.T) {
	s := setupScheme()
	// CR has status.secretRef set but the Secret doesn't exist
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default",
			Namespace: "team-alpha",
		},
		Spec: v1alpha1.APIKeySpec{
			Prefix: "sk-",
		},
		Status: v1alpha1.APIKeyStatus{
			SecretRef: &v1alpha1.SecretReference{
				Name: "llm-api-key",
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey).WithStatusSubresource(apiKey).Build()
	recorder := events.NewFakeRecorder(10)
	r := &APIKeyReconciler{Client: c, Recorder: recorder}

	// First reconcile: detect missing Secret, clear secretRef
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "default"},
	})
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter, "expected requeue when Secret is missing")
	requireEvent(t, recorder, `Warning SecretMissing Secret "llm-api-key" was not found; API key material will be regenerated`)

	// Verify secretRef was cleared
	var updated v1alpha1.APIKey
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated)
	assert.Nil(t, updated.Status.SecretRef, "expected status.secretRef to be cleared")
}

func TestReconcile_CorruptSecretRegenerates(t *testing.T) {
	s := setupScheme()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-api-key", Namespace: "team-alpha"},
		Data:       map[string][]byte{},
	}
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "team-alpha"},
		Spec:       v1alpha1.APIKeySpec{Prefix: "sk-"},
		Status: v1alpha1.APIKeyStatus{
			SecretRef: &v1alpha1.SecretReference{Name: "llm-api-key"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey, secret).WithStatusSubresource(apiKey).Build()
	recorder := events.NewFakeRecorder(10)
	r := &APIKeyReconciler{Client: c, Recorder: recorder}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "default"},
	})
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)
	requireEvent(t, recorder, `Warning SecretCorrupt Secret "llm-api-key" has no primary API key; API key material will be regenerated`)

	var updated v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated))
	assert.Nil(t, updated.Status.SecretRef)
}

func newDueRotationObjects(apiKeyUID types.UID) (*v1alpha1.APIKey, *corev1.Secret) {
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "default",
			Namespace:  "team-alpha",
			UID:        apiKeyUID,
			Finalizers: []string{finalizerName},
		},
		Spec: v1alpha1.APIKeySpec{
			Prefix: "sk-",
			Rotation: &v1alpha1.RotationSpec{
				RotateAt: &metav1.Time{Time: time.Now().Add(-time.Minute)},
			},
		},
		Status: v1alpha1.APIKeyStatus{
			Primary:   &v1alpha1.KeyStatus{KeyID: "oldkeyid"},
			Phase:     v1alpha1.PhaseActive,
			SecretRef: &v1alpha1.SecretReference{Name: "llm-api-key"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llm-api-key",
			Namespace: "team-alpha",
			Annotations: map[string]string{
				keyutil.PrimaryExpiryAnnotation: keyutil.NeverExpires,
				keyutil.PrimaryHashAnnotation:   keyutil.HashKey("sk-llmgw-Jx3kLmNpQrStUvWxYzAbCdEfGh0123456789AB"),
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(apiKey, v1alpha1.GroupVersion.WithKind("APIKey")),
			},
		},
		Data: map[string][]byte{
			keyutil.SecretKey: []byte("sk-llmgw-Jx3kLmNpQrStUvWxYzAbCdEfGh0123456789AB"),
		},
	}
	return apiKey, secret
}

func TestReconcile_Rotation(t *testing.T) {
	s := setupScheme()
	rawKey := "sk-llmgw-Jx3kLmNpQrStUvWxYzAbCdEfGh0123456789AB"
	oldKeyID := keyutil.KeyIDFromHash(keyutil.HashKey(rawKey))
	apiKeyUID := types.UID("rotation-uid")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llm-api-key",
			Namespace: "team-alpha",
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(&metav1.ObjectMeta{Name: "default", UID: apiKeyUID}, v1alpha1.GroupVersion.WithKind("APIKey")),
			},
		},
		Data: map[string][]byte{
			"apiKey": []byte(rawKey),
		},
	}
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default",
			Namespace: "team-alpha",
			UID:       apiKeyUID,
		},
		Spec: v1alpha1.APIKeySpec{
			Prefix: "sk-",
			Rotation: &v1alpha1.RotationSpec{
				RotateAt: &metav1.Time{Time: time.Now().Add(-1 * time.Minute)},
			},
		},
		Status: v1alpha1.APIKeyStatus{
			Primary: &v1alpha1.KeyStatus{
				KeyID: oldKeyID,
			},
			Phase: v1alpha1.PhaseActive,
			SecretRef: &v1alpha1.SecretReference{
				Name: "llm-api-key",
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey, secret).WithStatusSubresource(apiKey).Build()
	recorder := events.NewFakeRecorder(10)
	r := &APIKeyReconciler{Client: c, Recorder: recorder}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "default"},
	})
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter, "expected requeue after grace period")

	// Verify Secret has both keys
	var updatedSecret corev1.Secret
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"}, &updatedSecret)
	assert.Equal(t, rawKey, string(updatedSecret.Data["secondaryApiKey"]), "expected secondary key in secret to match original key")
	assert.NotEqual(t, rawKey, string(updatedSecret.Data["apiKey"]), "expected new key to be different from old key")
	assert.Equal(t, keyutil.HashKey(string(updatedSecret.Data[keyutil.SecretKey])), updatedSecret.Annotations[keyutil.PrimaryHashAnnotation])
	assert.Equal(t, keyutil.HashKey(rawKey), updatedSecret.Annotations[keyutil.SecondaryHashAnnotation])
	assert.NotEmpty(t, updatedSecret.Annotations[keyutil.RotationRequestAnnotation])
	assert.NotEmpty(t, updatedSecret.Annotations[keyutil.LastRotatedAtAnnotation])

	// Verify status
	var updated v1alpha1.APIKey
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated)
	assert.Equal(t, v1alpha1.PhaseActive, updated.Status.Phase)
	require.NotNil(t, updated.Status.Secondary, "expected status.secondaryKey to be set")
	assert.Equal(t, oldKeyID, updated.Status.Secondary.KeyID)
	assert.NotNil(t, updated.Status.LastRotatedAt, "expected status.lastRotatedAt to be set")
	assert.NotEqual(t, "oldkeyid", updated.Status.Primary.KeyID, "expected status.primaryKey.keyID to change after rotation")
	requireEvent(t, recorder, "Normal Rotated "+findReadyCondition(t, &updated).Message)

	// Verify spec.rotation.rotateAt was NOT mutated (controller should not write to spec)
	assert.NotNil(t, updated.Spec.Rotation, "expected spec.rotation to be preserved")
	assert.NotNil(t, updated.Spec.Rotation.RotateAt, "expected spec.rotation.rotateAt to be preserved (controller must not mutate spec)")

	// Verify lastRotatedAt >= rotateAt (rotation acknowledged via status)
	assert.False(t, updated.Status.LastRotatedAt.Time.Before(updated.Spec.Rotation.RotateAt.Time),
		"expected status.lastRotatedAt >= spec.rotation.rotateAt")
}

func TestReconcile_RotationSecretUpdateFailureEmitsEvent(t *testing.T) {
	s := setupScheme()
	apiKey, secret := newDueRotationObjects(types.UID("rotation-secret-failure-uid"))
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(apiKey, secret).
		WithStatusSubresource(apiKey).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, delegate client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					return assert.AnError
				}
				return delegate.Update(ctx, obj, opts...)
			},
		}).
		Build()
	recorder := &capturedEventRecorder{}
	r := &APIKeyReconciler{Client: c, Recorder: recorder}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "default"},
	})
	require.ErrorIs(t, err, assert.AnError)
	requireCapturedEvent(t, recorder, capturedEvent{
		EventType: corev1.EventTypeWarning,
		Reason:    eventReasonRotationFailed,
		Action:    eventActionRotateSecret,
		Note:      `Failed to update Secret "llm-api-key" with rotated API key material`,
	})
}

func TestReconcile_RotationStatusUpdateFailureEmitsEvent(t *testing.T) {
	s := setupScheme()
	apiKey, secret := newDueRotationObjects(types.UID("rotation-status-failure-uid"))
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(apiKey, secret).
		WithStatusSubresource(apiKey).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return assert.AnError
			},
		}).
		Build()
	recorder := &capturedEventRecorder{}
	r := &APIKeyReconciler{Client: c, Recorder: recorder}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "default"},
	})
	require.ErrorIs(t, err, assert.AnError)
	requireCapturedEvent(t, recorder, capturedEvent{
		EventType: corev1.EventTypeWarning,
		Reason:    eventReasonRotationFailed,
		Action:    eventActionPersistRotationStatus,
		Note:      `Failed to persist APIKey status after rotating Secret "llm-api-key"`,
	})
}

func TestReconcile_RecoversCompletedRotationAfterStatusFailure(t *testing.T) {
	s := setupScheme()
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	apiKey, secret := newDueRotationObjects(types.UID("rotation-status-recovery-uid"))
	oldRawKey := string(secret.Data[keyutil.SecretKey])
	oldKeyID := keyutil.KeyIDFromHash(keyutil.HashKey(oldRawKey))
	apiKey.Status.Primary.KeyID = oldKeyID
	statusUpdates := 0
	secretUpdates := 0
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(apiKey, secret).
		WithStatusSubresource(apiKey).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, delegate client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					secretUpdates++
				}
				return delegate.Update(ctx, obj, opts...)
			},
			SubResourceUpdate: func(ctx context.Context, delegate client.Client, subResource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				statusUpdates++
				if statusUpdates == 1 {
					return assert.AnError
				}
				return delegate.SubResource(subResource).Update(ctx, obj, opts...)
			},
		}).
		Build()
	fakeClock := clocktesting.NewFakeClock(now)
	r := &APIKeyReconciler{Client: c, Clock: fakeClock}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(apiKey)}

	_, err := r.Reconcile(context.Background(), request)
	require.ErrorIs(t, err, assert.AnError)
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), secret))
	rotatedPrimary := string(secret.Data[keyutil.SecretKey])
	require.NotEqual(t, oldRawKey, rotatedPrimary)
	assert.Equal(t, oldRawKey, string(secret.Data[keyutil.SecondarySecretKey]))
	assert.Equal(t, 1, secretUpdates)
	persistedRotationTime, err := keyutil.ParseExpiry(secret.Annotations[keyutil.LastRotatedAtAnnotation], false)
	require.NoError(t, err)
	assert.Equal(t, now, persistedRotationTime)

	fakeClock.Step(10 * time.Minute)
	_, err = r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), secret))
	assert.Equal(t, rotatedPrimary, string(secret.Data[keyutil.SecretKey]), "status recovery must not rotate the Secret again")
	assert.Equal(t, 1, secretUpdates)

	var updated v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), request.NamespacedName, &updated))
	require.NotNil(t, updated.Status.LastRotatedAt)
	assert.True(t, updated.Status.LastRotatedAt.Time.Equal(now), "recovery must preserve the persisted rotation completion time")
	assert.False(t, updated.Status.LastRotatedAt.Time.Equal(fakeClock.Now()), "recovery time must not replace rotation time")
	require.NotNil(t, updated.Status.Primary)
	assert.Equal(t, keyutil.KeyIDFromHash(keyutil.HashKey(rotatedPrimary)), updated.Status.Primary.KeyID)
	require.NotNil(t, updated.Status.Secondary)
	assert.Equal(t, oldKeyID, updated.Status.Secondary.KeyID)
}

func TestReconcile_NewerRotationRequestIsNotConsumedByStatusRecovery(t *testing.T) {
	s := setupScheme()
	firstRotationTime := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	apiKey, secret := newDueRotationObjects(types.UID("newer-rotation-request-uid"))
	apiKey.Spec.Rotation.RotateAt = &metav1.Time{Time: firstRotationTime.Add(-time.Minute)}
	oldRawKey := string(secret.Data[keyutil.SecretKey])
	apiKey.Status.Primary.KeyID = keyutil.KeyIDFromHash(keyutil.HashKey(oldRawKey))
	statusUpdates := 0
	secretUpdates := 0
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(apiKey, secret).
		WithStatusSubresource(apiKey).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, delegate client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					secretUpdates++
				}
				return delegate.Update(ctx, obj, opts...)
			},
			SubResourceUpdate: func(ctx context.Context, delegate client.Client, subResource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				statusUpdates++
				if statusUpdates == 1 {
					return assert.AnError
				}
				return delegate.SubResource(subResource).Update(ctx, obj, opts...)
			},
		}).
		Build()
	fakeClock := clocktesting.NewFakeClock(firstRotationTime)
	r := &APIKeyReconciler{Client: c, Clock: fakeClock}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(apiKey)}

	_, err := r.Reconcile(context.Background(), request)
	require.ErrorIs(t, err, assert.AnError)
	var afterFirst corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), &afterFirst))
	firstReplacement := string(afterFirst.Data[keyutil.SecretKey])
	require.NotEqual(t, oldRawKey, firstReplacement)
	assert.Equal(t, 1, secretUpdates)

	secondRotationTime := firstRotationTime.Add(5 * time.Minute)
	fakeClock.SetTime(secondRotationTime)
	var changedRequest v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(apiKey), &changedRequest))
	changedRequest.Spec.ExpiresIn = "2h"
	changedRequest.Spec.Rotation.RotateAt = &metav1.Time{Time: secondRotationTime.Add(-time.Minute)}
	require.NoError(t, c.Update(context.Background(), &changedRequest))

	_, err = r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), &afterFirst))
	assert.Equal(t, 2, secretUpdates, "the changed request must perform a second Secret rotation")
	assert.Equal(t, firstReplacement, string(afterFirst.Data[keyutil.SecondarySecretKey]))
	assert.NotEqual(t, firstReplacement, string(afterFirst.Data[keyutil.SecretKey]))
	secondRequestValue, err := keyutil.FormatExpiry(changedRequest.Spec.Rotation.RotateAt.Time)
	require.NoError(t, err)
	assert.Equal(t, secondRequestValue, afterFirst.Annotations[keyutil.RotationRequestAnnotation])
	primaryDeadline, err := keyutil.ParsePrimaryExpiry(afterFirst.Annotations)
	require.NoError(t, err)
	assert.Equal(t, secondRotationTime.Add(2*time.Hour), primaryDeadline)

	var updated v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), request.NamespacedName, &updated))
	require.NotNil(t, updated.Status.LastRotatedAt)
	assert.True(t, updated.Status.LastRotatedAt.Time.Equal(secondRotationTime))
}

func TestReconcile_RotationRetryUsesRefetchedKeyDeadline(t *testing.T) {
	s := setupScheme()
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	initialDeadline := now.Add(2 * time.Hour)
	refetchedDeadline := now.Add(20 * time.Minute)
	initialRaw := "sk-initial-generation"
	refetchedRaw := "sk-refetched-generation"
	uid := types.UID("rotation-refetch-generation")
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "team-alpha", UID: uid, Finalizers: []string{finalizerName}},
		Spec: v1alpha1.APIKeySpec{
			ExpiresIn: "3h",
			Rotation: &v1alpha1.RotationSpec{
				RotateAt:    &metav1.Time{Time: now.Add(-time.Minute)},
				GracePeriod: "1h",
			},
		},
		Status: v1alpha1.APIKeyStatus{
			Phase:     v1alpha1.PhaseActive,
			Primary:   &v1alpha1.KeyStatus{KeyID: keyutil.KeyIDFromHash(keyutil.HashKey(initialRaw)), ExpiresAt: &metav1.Time{Time: initialDeadline}},
			SecretRef: &v1alpha1.SecretReference{Name: keyutil.SecretName},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keyutil.SecretName,
			Namespace: apiKey.Namespace,
			Annotations: map[string]string{
				keyutil.PrimaryExpiryAnnotation: initialDeadline.Format(time.RFC3339Nano),
				keyutil.PrimaryHashAnnotation:   keyutil.HashKey(initialRaw),
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(apiKey, v1alpha1.GroupVersion.WithKind("APIKey"))},
		},
		Data: map[string][]byte{keyutil.SecretKey: []byte(initialRaw)},
	}

	secretGets := 0
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey, secret).WithStatusSubresource(apiKey).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, delegate client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if key == client.ObjectKeyFromObject(secret) {
					secretGets++
					if secretGets == 3 {
						var current corev1.Secret
						require.NoError(t, delegate.Get(ctx, key, &current))
						current.Data[keyutil.SecretKey] = []byte(refetchedRaw)
						current.Annotations[keyutil.PrimaryExpiryAnnotation] = refetchedDeadline.Format(time.RFC3339Nano)
						current.Annotations[keyutil.PrimaryHashAnnotation] = keyutil.HashKey(refetchedRaw)
						require.NoError(t, delegate.Update(ctx, &current))
					}
				}
				return delegate.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &APIKeyReconciler{Client: c, Clock: clocktesting.NewFakeClock(now)}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(apiKey)})
	require.NoError(t, err)
	require.GreaterOrEqual(t, secretGets, 3)
	var rotated corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), &rotated))
	assert.Equal(t, refetchedRaw, string(rotated.Data[keyutil.SecondarySecretKey]))
	assert.Equal(t, keyutil.HashKey(refetchedRaw), rotated.Annotations[keyutil.SecondaryHashAnnotation])
	secondaryDeadline, err := keyutil.ParseSecondaryExpiry(rotated.Annotations)
	require.NoError(t, err)
	assert.Equal(t, refetchedDeadline, secondaryDeadline, "retry must cap the secondary using the refetched key generation's deadline")
}

func TestReconcile_RotationAtPrimaryExpiryStillCreatesReplacement(t *testing.T) {
	s := setupScheme()
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	oldRaw := "sk-expiring-at-rotation"
	uid := types.UID("rotation-at-expiry")
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "team-alpha", UID: uid, Finalizers: []string{finalizerName}},
		Spec: v1alpha1.APIKeySpec{
			ExpiresIn: "1h",
			Rotation: &v1alpha1.RotationSpec{
				RotateAt:    &metav1.Time{Time: now},
				GracePeriod: "30m",
			},
		},
		Status: v1alpha1.APIKeyStatus{
			Phase:     v1alpha1.PhaseActive,
			Primary:   &v1alpha1.KeyStatus{KeyID: keyutil.KeyIDFromHash(keyutil.HashKey(oldRaw)), ExpiresAt: &metav1.Time{Time: now}},
			SecretRef: &v1alpha1.SecretReference{Name: keyutil.SecretName},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keyutil.SecretName,
			Namespace: apiKey.Namespace,
			Annotations: map[string]string{
				keyutil.PrimaryExpiryAnnotation: now.Format(time.RFC3339Nano),
				keyutil.PrimaryHashAnnotation:   keyutil.HashKey(oldRaw),
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(apiKey, v1alpha1.GroupVersion.WithKind("APIKey"))},
		},
		Data: map[string][]byte{keyutil.SecretKey: []byte(oldRaw)},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey, secret).WithStatusSubresource(apiKey).Build()
	r := &APIKeyReconciler{Client: c, Clock: clocktesting.NewFakeClock(now)}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(apiKey)})
	require.NoError(t, err)
	var rotated corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), &rotated))
	assert.NotEqual(t, oldRaw, string(rotated.Data[keyutil.SecretKey]))
	assert.Equal(t, oldRaw, string(rotated.Data[keyutil.SecondarySecretKey]))
	primaryDeadline, err := keyutil.ParsePrimaryExpiry(rotated.Annotations)
	require.NoError(t, err)
	assert.Equal(t, now.Add(time.Hour), primaryDeadline)
	secondaryDeadline, err := keyutil.ParseSecondaryExpiry(rotated.Annotations)
	require.NoError(t, err)
	assert.Equal(t, now, secondaryDeadline, "the old key remains expired at the equality boundary")

	var updated v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(apiKey), &updated))
	assert.Equal(t, v1alpha1.PhaseActive, updated.Status.Phase)
	assert.NotNil(t, updated.Status.SecretRef)
}

func TestReconcile_RotationWithNewExpiresIn(t *testing.T) {
	s := setupScheme()
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	oldPrimaryExpiresAt := now.Add(30 * time.Minute)
	rawKey := "sk-llmgw-Jx3kLmNpQrStUvWxYzAbCdEfGh0123456789AB"
	apiKeyUID := types.UID("rotation-expires-uid")
	oldPrimaryExpiryValue, err := keyutil.FormatExpiry(oldPrimaryExpiresAt)
	require.NoError(t, err)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "llm-api-key",
			Namespace:   "team-alpha",
			Annotations: map[string]string{keyutil.PrimaryExpiryAnnotation: oldPrimaryExpiryValue},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(&metav1.ObjectMeta{Name: "default", UID: apiKeyUID}, v1alpha1.GroupVersion.WithKind("APIKey")),
			},
		},
		Data: map[string][]byte{
			"apiKey": []byte(rawKey),
		},
	}
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "default",
			Namespace:         "team-alpha",
			UID:               apiKeyUID,
			CreationTimestamp: metav1.NewTime(now.Add(-47*time.Hour - 30*time.Minute)),
		},
		Spec: v1alpha1.APIKeySpec{
			Prefix:    "sk-",
			ExpiresIn: "48h", // New expiration for the rotated key
			Rotation: &v1alpha1.RotationSpec{
				RotateAt: &metav1.Time{Time: now.Add(-1 * time.Minute)},
			},
		},
		Status: v1alpha1.APIKeyStatus{
			Primary: &v1alpha1.KeyStatus{
				KeyID: "oldkeyid",
			},
			Phase: v1alpha1.PhaseActive,
			SecretRef: &v1alpha1.SecretReference{
				Name: "llm-api-key",
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey, secret).WithStatusSubresource(apiKey).Build()
	r := &APIKeyReconciler{Client: c, Clock: clocktesting.NewFakeClock(now)}

	_, err = r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "default"},
	})
	require.NoError(t, err)

	var updated v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated))
	require.NotNil(t, updated.Status.Primary, "expected primary to be set")
	require.NotNil(t, updated.Status.Primary.ExpiresAt)
	assert.True(t, updated.Status.Primary.ExpiresAt.Time.Equal(now.Add(48*time.Hour)))
	require.NotNil(t, updated.Status.Secondary)
	assert.True(t, updated.Status.Secondary.ExpiresAt.Time.Equal(oldPrimaryExpiresAt), "rotation must not extend the old primary beyond its finite expiry")

	var updatedSecret corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), &updatedSecret))
	primaryDeadline, err := keyutil.ParsePrimaryExpiry(updatedSecret.Annotations)
	require.NoError(t, err)
	assert.Equal(t, now.Add(48*time.Hour), primaryDeadline)
	secondaryDeadline, err := keyutil.ParseSecondaryExpiry(updatedSecret.Annotations)
	require.NoError(t, err)
	assert.Equal(t, oldPrimaryExpiresAt, secondaryDeadline)
}

func TestReconcile_RotationAppliesExpiresInOnlyToReplacement(t *testing.T) {
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	finiteOldExpiry := now.Add(20 * time.Minute)
	tests := []struct {
		name                   string
		oldPrimaryExpiresAt    *time.Time
		newExpiresIn           string
		wantPrimaryExpiresAt   *time.Time
		wantSecondaryExpiresAt time.Time
	}{
		{
			name:                   "finite current key to non-expiring replacement",
			oldPrimaryExpiresAt:    &finiteOldExpiry,
			newExpiresIn:           "",
			wantPrimaryExpiresAt:   nil,
			wantSecondaryExpiresAt: finiteOldExpiry,
		},
		{
			name:                   "non-expiring current key to short finite replacement",
			oldPrimaryExpiresAt:    nil,
			newExpiresIn:           "5m",
			wantPrimaryExpiresAt:   func() *time.Time { value := now.Add(5 * time.Minute); return &value }(),
			wantSecondaryExpiresAt: now.Add(5 * time.Minute),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := setupScheme()
			uid := types.UID("rotation-expiry-transition-" + tt.name)
			rawKey := "sk-current-key-" + tt.name
			primaryValue, err := keyutil.FormatPrimaryExpiry(tt.oldPrimaryExpiresAt)
			require.NoError(t, err)
			currentKeyID := keyutil.KeyIDFromHash(keyutil.HashKey(rawKey))
			apiKey := &v1alpha1.APIKey{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "team-alpha", UID: uid},
				Spec: v1alpha1.APIKeySpec{
					Prefix:    "sk-",
					ExpiresIn: tt.newExpiresIn,
					Rotation: &v1alpha1.RotationSpec{
						RotateAt:    &metav1.Time{Time: now.Add(-time.Minute)},
						GracePeriod: "2h",
					},
				},
				Status: v1alpha1.APIKeyStatus{
					Phase:     v1alpha1.PhaseActive,
					Primary:   &v1alpha1.KeyStatus{KeyID: currentKeyID},
					SecretRef: &v1alpha1.SecretReference{Name: keyutil.SecretName},
				},
			}
			if tt.oldPrimaryExpiresAt != nil {
				apiKey.Status.Primary.ExpiresAt = &metav1.Time{Time: *tt.oldPrimaryExpiresAt}
			}
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:            keyutil.SecretName,
					Namespace:       apiKey.Namespace,
					Annotations:     map[string]string{keyutil.PrimaryExpiryAnnotation: primaryValue},
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(apiKey, v1alpha1.GroupVersion.WithKind("APIKey"))},
				},
				Data: map[string][]byte{keyutil.SecretKey: []byte(rawKey)},
			}
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey, secret).WithStatusSubresource(apiKey).Build()
			r := &APIKeyReconciler{Client: c, Clock: clocktesting.NewFakeClock(now)}

			_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(apiKey)})
			require.NoError(t, err)
			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), secret))
			primaryDeadline, err := keyutil.ParsePrimaryExpiry(secret.Annotations)
			require.NoError(t, err)
			if tt.wantPrimaryExpiresAt == nil {
				assert.True(t, primaryDeadline.IsZero())
			} else {
				assert.True(t, primaryDeadline.Equal(*tt.wantPrimaryExpiresAt))
			}
			secondaryDeadline, err := keyutil.ParseSecondaryExpiry(secret.Annotations)
			require.NoError(t, err)
			assert.True(t, secondaryDeadline.Equal(tt.wantSecondaryExpiresAt))
			if !primaryDeadline.IsZero() {
				assert.False(t, secondaryDeadline.After(primaryDeadline), "secondary must not outlive the replacement primary")
			}
		})
	}
}

func TestReconcile_ValidSecretExpiryMetadataWinsOverSpecAndStatus(t *testing.T) {
	s := setupScheme()
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	primaryDeadline := now.Add(2 * time.Hour)
	secondaryDeadline := now.Add(time.Hour)
	primaryValue, err := keyutil.FormatExpiry(primaryDeadline)
	require.NoError(t, err)
	secondaryValue, err := keyutil.FormatExpiry(secondaryDeadline)
	require.NoError(t, err)
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "team-alpha", UID: types.UID("secret-metadata-authority")},
		Spec: v1alpha1.APIKeySpec{
			ExpiresIn: "5m",
			Rotation:  &v1alpha1.RotationSpec{RotateAt: &metav1.Time{Time: now.Add(30 * time.Minute)}},
		},
		Status: v1alpha1.APIKeyStatus{
			Phase: v1alpha1.PhaseActive,
			Primary: &v1alpha1.KeyStatus{
				KeyID:     "stale-primary-id",
				ExpiresAt: &metav1.Time{Time: now.Add(-time.Hour)},
			},
			Secondary: &v1alpha1.KeyStatus{
				KeyID:     "stale-secondary-id",
				ExpiresAt: &metav1.Time{Time: now.Add(4 * time.Hour)},
			},
			SecretRef: &v1alpha1.SecretReference{Name: keyutil.SecretName},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keyutil.SecretName,
			Namespace: apiKey.Namespace,
			Annotations: map[string]string{
				keyutil.PrimaryExpiryAnnotation:   primaryValue,
				keyutil.SecondaryExpiryAnnotation: secondaryValue,
				keyutil.PrimaryHashAnnotation:     keyutil.HashKey("sk-authoritative-primary"),
				keyutil.SecondaryHashAnnotation:   keyutil.HashKey("sk-authoritative-secondary"),
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(apiKey, v1alpha1.GroupVersion.WithKind("APIKey"))},
		},
		Data: map[string][]byte{
			keyutil.SecretKey:          []byte("sk-authoritative-primary"),
			keyutil.SecondarySecretKey: []byte("sk-authoritative-secondary"),
		},
	}
	secretUpdates := 0
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey, secret).WithStatusSubresource(apiKey).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, delegate client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					secretUpdates++
				}
				return delegate.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &APIKeyReconciler{Client: c, Clock: clocktesting.NewFakeClock(now)}

	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(apiKey)})
	require.NoError(t, err)
	assert.Zero(t, secretUpdates, "valid authoritative annotations must not be rewritten")
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), secret))
	assert.Equal(t, primaryValue, secret.Annotations[keyutil.PrimaryExpiryAnnotation])
	assert.Equal(t, secondaryValue, secret.Annotations[keyutil.SecondaryExpiryAnnotation])

	var updated v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(apiKey), &updated))
	require.NotNil(t, updated.Status.Primary)
	require.NotNil(t, updated.Status.Primary.ExpiresAt)
	assert.True(t, updated.Status.Primary.ExpiresAt.Equal(&metav1.Time{Time: primaryDeadline}))
	require.NotNil(t, updated.Status.Secondary)
	require.NotNil(t, updated.Status.Secondary.ExpiresAt)
	assert.True(t, updated.Status.Secondary.ExpiresAt.Equal(&metav1.Time{Time: secondaryDeadline}))
}

func TestReconcile_FractionalClockProducesStableWholeSecondMetadata(t *testing.T) {
	s := setupScheme()
	now := time.Date(2026, time.October, 8, 12, 0, 0, 987654321, time.FixedZone("offset", 11*60*60))
	expectedNow := now.UTC().Truncate(time.Second)
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "default",
			Namespace:         "team-alpha",
			UID:               types.UID("fractional-clock"),
			CreationTimestamp: metav1.NewTime(now),
		},
		Spec: v1alpha1.APIKeySpec{ExpiresIn: "2h"},
	}
	secretUpdates := 0
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey).WithStatusSubresource(apiKey).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, delegate client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					secretUpdates++
				}
				return delegate.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &APIKeyReconciler{Client: c, Clock: clocktesting.NewFakeClock(now)}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(apiKey)}

	_, err := r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	var secret corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: apiKey.Namespace, Name: keyutil.SecretName}, &secret))
	primaryDeadline, err := keyutil.ParsePrimaryExpiry(secret.Annotations)
	require.NoError(t, err)
	assert.Equal(t, expectedNow.Add(2*time.Hour), primaryDeadline)
	secondaryDeadline, err := keyutil.ParseSecondaryExpiry(secret.Annotations)
	require.NoError(t, err)
	assert.Equal(t, expectedNow.Add(2*time.Hour), secondaryDeadline)
	assert.Zero(t, primaryDeadline.Nanosecond())
	assert.Zero(t, secondaryDeadline.Nanosecond())

	_, err = r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	_, err = r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	assert.Zero(t, secretUpdates, "API round trips must not cause expiry metadata rewrites")
}

func TestReconcile_GracePeriodExpiry(t *testing.T) {
	s := setupScheme()
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	rawKey := "sk-llmgw-Jx3kLmNpQrStUvWxYzAbCdEfGh0123456789AB"
	secondaryKey := "sk-llmgw-OldKeyOldKeyOldKeyOldKeyOldKeyABCD"
	apiKeyUID := types.UID("grace-period-uid")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llm-api-key",
			Namespace: "team-alpha",
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(&metav1.ObjectMeta{Name: "default", UID: apiKeyUID}, v1alpha1.GroupVersion.WithKind("APIKey")),
			},
		},
		Data: map[string][]byte{
			"apiKey":          []byte(rawKey),
			"secondaryApiKey": []byte(secondaryKey),
		},
	}
	pastTime := metav1.NewTime(now)
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default",
			Namespace: "team-alpha",
			UID:       apiKeyUID,
		},
		Spec: v1alpha1.APIKeySpec{
			Prefix: "sk-",
		},
		Status: v1alpha1.APIKeyStatus{
			Primary: &v1alpha1.KeyStatus{
				KeyID: keyutil.KeyIDFromHash(keyutil.HashKey(rawKey)),
			},
			Phase: v1alpha1.PhaseActive,
			SecretRef: &v1alpha1.SecretReference{
				Name: "llm-api-key",
			},
			Secondary: &v1alpha1.KeyStatus{
				KeyID:     "prevkeyid",
				ExpiresAt: &metav1.Time{Time: pastTime.Time}, // Grace period already expired
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey, secret).WithStatusSubresource(apiKey).Build()
	recorder := events.NewFakeRecorder(10)
	r := &APIKeyReconciler{Client: c, Recorder: recorder, Clock: clocktesting.NewFakeClock(now)}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "default"},
	})
	require.NoError(t, err)
	requireEvent(t, recorder, `Normal SecondaryKeyRemoved Removed expired secondary API key from Secret "llm-api-key"`)

	// Verify status.secondaryKey was cleared
	var updated v1alpha1.APIKey
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated)
	assert.Nil(t, updated.Status.Secondary, "expected status.secondaryKey to be cleared after grace period")

	// Verify secondaryApiKey was removed from Secret
	var updatedSecret corev1.Secret
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"}, &updatedSecret)
	_, hasPrevKey := updatedSecret.Data[keyutil.SecondarySecretKey]
	assert.False(t, hasPrevKey, "expected secondaryApiKey to be removed from Secret")
	_, hasSecondaryExpiry := updatedSecret.Annotations[keyutil.SecondaryExpiryAnnotation]
	assert.False(t, hasSecondaryExpiry, "expected secondary expiry annotation to be removed atomically")
	_, hasSecondaryHash := updatedSecret.Annotations[keyutil.SecondaryHashAnnotation]
	assert.False(t, hasSecondaryHash, "expected secondary hash binding to be removed atomically")
}

func TestReconcile_RekeysLegacyCreationGapWithoutPrimaryStatus(t *testing.T) {
	s := setupScheme()
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	createdAt := now.Add(-30 * time.Minute)
	originalPrimary := "sk-legacy-creation-primary"
	originalSecondary := "sk-legacy-creation-secondary"
	uid := types.UID("legacy-creation-gap")
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "default",
			Namespace:         "team-alpha",
			UID:               uid,
			CreationTimestamp: metav1.NewTime(createdAt),
			Finalizers:        []string{finalizerName},
		},
		Spec: v1alpha1.APIKeySpec{ExpiresIn: "2h"},
		Status: v1alpha1.APIKeyStatus{
			Phase: v1alpha1.PhasePending,
			Secondary: &v1alpha1.KeyStatus{
				KeyID:     keyutil.KeyIDFromHash(keyutil.HashKey(originalSecondary)),
				ExpiresAt: &metav1.Time{Time: now.Add(time.Hour)},
			},
			SecretRef: &v1alpha1.SecretReference{Name: keyutil.SecretName},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            keyutil.SecretName,
			Namespace:       apiKey.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(apiKey, v1alpha1.GroupVersion.WithKind("APIKey"))},
		},
		Data: map[string][]byte{
			keyutil.SecretKey:          []byte(originalPrimary),
			keyutil.SecondarySecretKey: []byte(originalSecondary),
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey, secret).WithStatusSubresource(apiKey).Build()
	r := &APIKeyReconciler{Client: c, Clock: clocktesting.NewFakeClock(now)}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(apiKey)})
	require.NoError(t, err)
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), secret))
	newPrimary := string(secret.Data[keyutil.SecretKey])
	newSecondary := string(secret.Data[keyutil.SecondarySecretKey])
	assert.NotEqual(t, originalPrimary, newPrimary)
	assert.NotEqual(t, originalSecondary, newSecondary)
	assert.Equal(t, keyutil.HashKey(newPrimary), secret.Annotations[keyutil.PrimaryHashAnnotation])
	assert.Equal(t, keyutil.HashKey(newSecondary), secret.Annotations[keyutil.SecondaryHashAnnotation])
	primaryDeadline, err := keyutil.ParsePrimaryExpiry(secret.Annotations)
	require.NoError(t, err)
	assert.Equal(t, createdAt.Add(2*time.Hour), primaryDeadline, "creation-gap recovery must preserve the original creation-based lifetime")

	var updated v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(apiKey), &updated))
	require.NotNil(t, updated.Status.Primary)
	assert.Equal(t, keyutil.KeyIDFromHash(keyutil.HashKey(newPrimary)), updated.Status.Primary.KeyID)
	assert.Equal(t, v1alpha1.PhaseActive, updated.Status.Phase)
}

func TestReconcile_RekeysAmbiguousLegacyPartialRotation(t *testing.T) {
	s := setupScheme()
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	oldPrimary := "sk-old-primary-now-secondary"
	unpersistedReplacement := "sk-unpersisted-replacement"
	uid := types.UID("legacy-partial-rotation")
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "team-alpha", UID: uid, CreationTimestamp: metav1.NewTime(now.Add(-time.Hour)), Finalizers: []string{finalizerName}},
		Spec:       v1alpha1.APIKeySpec{ExpiresIn: "2h"},
		Status: v1alpha1.APIKeyStatus{
			Phase: v1alpha1.PhaseActive,
			Primary: &v1alpha1.KeyStatus{
				KeyID:     keyutil.KeyIDFromHash(keyutil.HashKey(oldPrimary)),
				ExpiresAt: &metav1.Time{Time: now.Add(time.Hour)},
			},
			SecretRef: &v1alpha1.SecretReference{Name: keyutil.SecretName},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            keyutil.SecretName,
			Namespace:       apiKey.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(apiKey, v1alpha1.GroupVersion.WithKind("APIKey"))},
		},
		Data: map[string][]byte{
			keyutil.SecretKey:          []byte(unpersistedReplacement),
			keyutil.SecondarySecretKey: []byte(oldPrimary),
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey, secret).WithStatusSubresource(apiKey).Build()
	r := &APIKeyReconciler{Client: c, Clock: clocktesting.NewFakeClock(now)}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(apiKey)})
	require.NoError(t, err)
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), secret))
	newPrimary := string(secret.Data[keyutil.SecretKey])
	newSecondary := string(secret.Data[keyutil.SecondarySecretKey])
	assert.NotEqual(t, unpersistedReplacement, newPrimary)
	assert.NotEqual(t, oldPrimary, newPrimary)
	assert.NotEqual(t, unpersistedReplacement, newSecondary)
	assert.NotEqual(t, oldPrimary, newSecondary)
	assert.Equal(t, keyutil.HashKey(newPrimary), secret.Annotations[keyutil.PrimaryHashAnnotation])
	assert.Equal(t, keyutil.HashKey(newSecondary), secret.Annotations[keyutil.SecondaryHashAnnotation])
	primaryDeadline, err := keyutil.ParsePrimaryExpiry(secret.Annotations)
	require.NoError(t, err)
	assert.Equal(t, now.Add(2*time.Hour), primaryDeadline, "ambiguous established material must receive a fresh replacement lifetime")
}

func TestReconcile_BackfillsLegacySecretWithoutExtendingSecondary(t *testing.T) {
	s := setupScheme()
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "team-alpha", UID: types.UID("backfill-never-uid")},
		Status: v1alpha1.APIKeyStatus{
			Phase:     v1alpha1.PhaseActive,
			Primary:   &v1alpha1.KeyStatus{KeyID: keyutil.KeyIDFromHash(keyutil.HashKey("sk-primary"))},
			SecretRef: &v1alpha1.SecretReference{Name: keyutil.SecretName},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keyutil.SecretName,
			Namespace: apiKey.Namespace,
			Annotations: map[string]string{
				"example.com/preserved": "true",
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(apiKey, v1alpha1.GroupVersion.WithKind("APIKey"))},
		},
		Data: map[string][]byte{
			keyutil.SecretKey:          []byte("sk-primary"),
			keyutil.SecondarySecretKey: []byte("sk-legacy-secondary"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey, secret).WithStatusSubresource(apiKey).Build()
	r := &APIKeyReconciler{Client: c, Clock: clocktesting.NewFakeClock(now)}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(apiKey)})
	require.NoError(t, err)
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), secret))
	assert.Equal(t, keyutil.NeverExpires, secret.Annotations[keyutil.PrimaryExpiryAnnotation])
	assert.Equal(t, keyutil.HashKey("sk-primary"), secret.Annotations[keyutil.PrimaryHashAnnotation])
	assert.Equal(t, "true", secret.Annotations["example.com/preserved"])
	_, hasSecondary := secret.Data[keyutil.SecondarySecretKey]
	assert.False(t, hasSecondary, "legacy secondary without authoritative status expiry must be removed")
	_, hasSecondaryExpiry := secret.Annotations[keyutil.SecondaryExpiryAnnotation]
	assert.False(t, hasSecondaryExpiry)
}

func TestReconcile_BackfillCapsSecondaryAtPrimaryExpiry(t *testing.T) {
	s := setupScheme()
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	createdAt := now.Add(-time.Hour)
	primaryExpiresAt := now.Add(time.Hour)
	authoritativeSecondary := now.Add(4 * time.Hour)
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "default",
			Namespace:         "team-alpha",
			UID:               types.UID("backfill-cap-uid"),
			CreationTimestamp: metav1.NewTime(createdAt),
		},
		Spec: v1alpha1.APIKeySpec{ExpiresIn: "2h"},
		Status: v1alpha1.APIKeyStatus{
			Phase:     v1alpha1.PhaseActive,
			Primary:   &v1alpha1.KeyStatus{KeyID: keyutil.KeyIDFromHash(keyutil.HashKey("sk-primary")), ExpiresAt: &metav1.Time{Time: primaryExpiresAt}},
			Secondary: &v1alpha1.KeyStatus{KeyID: keyutil.KeyIDFromHash(keyutil.HashKey("sk-secondary")), ExpiresAt: &metav1.Time{Time: authoritativeSecondary}},
			SecretRef: &v1alpha1.SecretReference{Name: keyutil.SecretName},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            keyutil.SecretName,
			Namespace:       apiKey.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(apiKey, v1alpha1.GroupVersion.WithKind("APIKey"))},
		},
		Data: map[string][]byte{
			keyutil.SecretKey:          []byte("sk-primary"),
			keyutil.SecondarySecretKey: []byte("sk-secondary"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey, secret).WithStatusSubresource(apiKey).Build()
	r := &APIKeyReconciler{Client: c, Clock: clocktesting.NewFakeClock(now)}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(apiKey)})
	require.NoError(t, err)
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), secret))
	primaryDeadline, err := keyutil.ParsePrimaryExpiry(secret.Annotations)
	require.NoError(t, err)
	assert.Equal(t, primaryExpiresAt, primaryDeadline)
	secondaryDeadline, err := keyutil.ParseSecondaryExpiry(secret.Annotations)
	require.NoError(t, err)
	assert.Equal(t, primaryExpiresAt, secondaryDeadline)
	assert.Equal(t, keyutil.HashKey("sk-primary"), secret.Annotations[keyutil.PrimaryHashAnnotation])
	assert.Equal(t, keyutil.HashKey("sk-secondary"), secret.Annotations[keyutil.SecondaryHashAnnotation])
	var updated v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(apiKey), &updated))
	assert.True(t, updated.Status.Secondary.ExpiresAt.Time.Equal(primaryExpiresAt))
}

func TestReconcile_BackfillAvoidsNoopSecretUpdate(t *testing.T) {
	s := setupScheme()
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "team-alpha", UID: types.UID("backfill-noop-uid"), Finalizers: []string{finalizerName}},
		Status: v1alpha1.APIKeyStatus{
			Phase:     v1alpha1.PhaseActive,
			Primary:   &v1alpha1.KeyStatus{KeyID: keyutil.KeyIDFromHash(keyutil.HashKey("sk-primary"))},
			SecretRef: &v1alpha1.SecretReference{Name: keyutil.SecretName},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keyutil.SecretName,
			Namespace: apiKey.Namespace,
			Annotations: map[string]string{
				keyutil.PrimaryExpiryAnnotation: keyutil.NeverExpires,
				keyutil.PrimaryHashAnnotation:   keyutil.HashKey("sk-primary"),
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(apiKey, v1alpha1.GroupVersion.WithKind("APIKey"))},
		},
		Data: map[string][]byte{keyutil.SecretKey: []byte("sk-primary")},
	}
	secretUpdates := 0
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey, secret).WithStatusSubresource(apiKey).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, delegate client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					secretUpdates++
				}
				return delegate.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &APIKeyReconciler{Client: c, Clock: clocktesting.NewFakeClock(now)}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(apiKey)})
	require.NoError(t, err)
	assert.Zero(t, secretUpdates)
}

func TestReconcile_SecretConflictNotOwned(t *testing.T) {
	s := setupScheme()

	// Pre-existing Secret NOT owned by any APIKey
	unownedSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llm-api-key",
			Namespace: "team-alpha",
		},
		Data: map[string][]byte{
			"apiKey": []byte("someone-elses-key"),
		},
	}

	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default",
			Namespace: "team-alpha",
			UID:       "test-uid-123",
		},
		Spec: v1alpha1.APIKeySpec{
			Prefix: "sk-",
		},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey, unownedSecret).WithStatusSubresource(apiKey).Build()
	recorder := events.NewFakeRecorder(10)
	r := &APIKeyReconciler{Client: c, Recorder: recorder}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "default"}}

	_, err := r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	requireEvent(t, recorder, `Warning SecretConflict Secret "llm-api-key" already exists but is not owned by this APIKey`)

	// Verify the unowned Secret data was NOT overwritten
	var secret corev1.Secret
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"}, &secret)
	assert.Equal(t, "someone-elses-key", string(secret.Data["apiKey"]), "expected unowned Secret data to remain unchanged")

	// Verify APIKey is marked NotReady with SecretConflict
	var updated v1alpha1.APIKey
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated)
	assert.Equal(t, v1alpha1.PhasePending, updated.Status.Phase)

	_, err = r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	requireNoEvent(t, recorder)
}

func TestGracePeriod_Default(t *testing.T) {
	r := &APIKeyReconciler{}

	// No rotation config => the 24h default applies. Asserted against the
	// literal so a change to the constant has to be made deliberately.
	apiKey := &v1alpha1.APIKey{
		Spec: v1alpha1.APIKeySpec{Prefix: "sk-"},
	}
	assert.Equal(t, 24*time.Hour, v1alpha1.DefaultGracePeriod)
	assert.Equal(t, v1alpha1.DefaultGracePeriod, r.gracePeriod(apiKey))

	// Empty rotation struct (no GracePeriod set) => still default.
	apiKey.Spec.Rotation = &v1alpha1.RotationSpec{}
	assert.Equal(t, v1alpha1.DefaultGracePeriod, r.gracePeriod(apiKey))

	// Unparseable GracePeriod => fall back to default.
	apiKey.Spec.Rotation.GracePeriod = "not-a-duration"
	assert.Equal(t, v1alpha1.DefaultGracePeriod, r.gracePeriod(apiKey))

	// Explicit GracePeriod overrides default.
	apiKey.Spec.Rotation.GracePeriod = "30m"
	assert.Equal(t, 30*time.Minute, r.gracePeriod(apiKey))
}

func TestReconcile_SecondaryKeyPersistsWithinDefaultGracePeriod(t *testing.T) {
	s := setupScheme()
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default",
			Namespace: "team-alpha",
		},
		Spec: v1alpha1.APIKeySpec{
			Prefix: "sk-",
		},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey).WithStatusSubresource(apiKey).Build()
	r := &APIKeyReconciler{Client: c}

	// First reconcile creates the Secret + both keys with the default grace period.
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "default"},
	})
	require.NoError(t, err)

	var updated v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated))
	require.NotNil(t, updated.Status.Secondary, "expected secondary key to be set on creation")
	require.NotNil(t, updated.Status.Secondary.ExpiresAt, "expected secondary.expiresAt to be set")
	// The secondary generated at creation expires one grace period out, so a
	// key that is never rotated loses its secondary after 24h. See the
	// spec.rotation.gracePeriod field docs.
	assert.WithinDuration(t, time.Now().Add(v1alpha1.DefaultGracePeriod),
		updated.Status.Secondary.ExpiresAt.Time, time.Minute,
		"expected secondary.expiresAt one default grace period from creation")

	// Second reconcile, still well within the grace period: the secondary key
	// must NOT be cleaned up.
	_, err = r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "default"},
	})
	require.NoError(t, err)

	var after v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &after))
	assert.NotNil(t, after.Status.Secondary, "secondary key should remain under default grace period")

	var secret corev1.Secret
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"}, &secret))
	_, hasPrev := secret.Data["secondaryApiKey"]
	assert.True(t, hasPrev, "secondaryApiKey should remain in Secret under default grace period")
}
