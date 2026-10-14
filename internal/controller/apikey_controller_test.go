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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/kaito-project/llm-gateway-auth/api/v1alpha1"
	"github.com/kaito-project/llm-gateway-auth/internal/keyutil"
)

func setupScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	return scheme
}

func reconcileRequest() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: keyutil.SingletonName}}
}

func operatorKeyID(raw string) string {
	return keyutil.KeyIDFromHash(keyutil.HashKey(raw))
}

func assertSameTime(t *testing.T, expected, actual time.Time, msgAndArgs ...any) {
	t.Helper()
	if len(msgAndArgs) > 0 {
		assert.True(t, expected.Equal(actual), msgAndArgs...)
		return
	}
	assert.True(t, expected.Equal(actual), "expected %s, got %s", expected, actual)
}

func coherentObjects(now time.Time, rawPrimary string, expiresAt *time.Time) (*v1alpha1.APIKey, *corev1.Secret) {
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:              keyutil.SingletonName,
			Namespace:         "team-alpha",
			UID:               types.UID("apikey-uid"),
			Finalizers:        []string{finalizerName},
			CreationTimestamp: metav1.NewTime(now.Add(-time.Hour)),
		},
		Spec: v1alpha1.APIKeySpec{Prefix: "sk-"},
		Status: v1alpha1.APIKeyStatus{
			Phase:     v1alpha1.PhaseActive,
			Primary:   &v1alpha1.KeyStatus{KeyID: operatorKeyID(rawPrimary)},
			SecretRef: &v1alpha1.SecretReference{Name: keyutil.SecretName},
			Conditions: []metav1.Condition{{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				Reason:             "Synced",
				Message:            "Key is active",
				LastTransitionTime: metav1.NewTime(now.Add(-time.Hour)),
			}},
		},
	}
	if expiresAt != nil {
		apiKey.Status.Primary.ExpiresAt = &metav1.Time{Time: *expiresAt}
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keyutil.SecretName,
			Namespace: apiKey.Namespace,
			UID:       types.UID("secret-uid"),
			Labels: map[string]string{
				managedByMetadataKey: managedByValue,
			},
			Annotations: map[string]string{
				managedByMetadataKey: managedByValue,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(apiKey, v1alpha1.GroupVersion.WithKind("APIKey")),
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{keyutil.SecretKey: []byte(rawPrimary)},
	}
	return apiKey, secret
}

func newTestReconciler(now time.Time, objects ...client.Object) (*APIKeyReconciler, client.Client) {
	builder := fake.NewClientBuilder().WithScheme(setupScheme()).WithStatusSubresource(&v1alpha1.APIKey{})
	if len(objects) > 0 {
		builder = builder.WithObjects(objects...)
	}
	c := builder.Build()
	return &APIKeyReconciler{Client: c, Clock: clocktesting.NewFakeClock(now)}, c
}

func TestReconcile_NotFound(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	r, _ := newTestReconciler(now)
	_, err := r.Reconcile(context.Background(), reconcileRequest())
	require.NoError(t, err)
}

func TestReconcile_CreationPublishesCompleteStatusWithManagedMetadata(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:              keyutil.SingletonName,
			Namespace:         "team-alpha",
			UID:               types.UID("apikey-uid"),
			CreationTimestamp: metav1.NewTime(now),
		},
		Spec: v1alpha1.APIKeySpec{
			Prefix:    "sk-",
			ExpiresIn: "20m",
			Rotation:  &v1alpha1.RotationSpec{GracePeriod: "1h"},
		},
	}
	r, c := newTestReconciler(now, apiKey)

	result, err := r.Reconcile(context.Background(), reconcileRequest())
	require.NoError(t, err)
	assert.Equal(t, time.Second, result.RequeueAfter)

	var secret corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: apiKey.Namespace, Name: keyutil.SecretName}, &secret))
	assert.Equal(t, managedByValue, secret.Annotations[managedByMetadataKey])
	assert.Equal(t, managedByValue, secret.Labels[managedByMetadataKey])
	assert.NotEmpty(t, secret.Data[keyutil.SecretKey])
	assert.NotEmpty(t, secret.Data[keyutil.SecondarySecretKey])

	var updated v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), reconcileRequest().NamespacedName, &updated))
	assert.Equal(t, v1alpha1.PhaseActive, updated.Status.Phase)
	require.NotNil(t, updated.Status.SecretRef)
	assert.Equal(t, keyutil.SecretName, updated.Status.SecretRef.Name)
	require.NotNil(t, updated.Status.Primary)
	assert.Equal(t, operatorKeyID(string(secret.Data[keyutil.SecretKey])), updated.Status.Primary.KeyID)
	require.NotNil(t, updated.Status.Primary.ExpiresAt)
	assertSameTime(t, now.Add(20*time.Minute), updated.Status.Primary.ExpiresAt.Time)
	require.NotNil(t, updated.Status.Secondary)
	assert.Equal(t, operatorKeyID(string(secret.Data[keyutil.SecondarySecretKey])), updated.Status.Secondary.KeyID)
	require.NotNil(t, updated.Status.Secondary.ExpiresAt)
	assertSameTime(t, now.Add(20*time.Minute), updated.Status.Secondary.ExpiresAt.Time, "secondary must not outlive the primary")
}

func TestReconcile_SpecChangeDoesNotAlterCurrentGenerationDeadline(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	persistedExpiry := now.Add(45 * time.Minute)
	apiKey, secret := coherentObjects(now, "sk-current", &persistedExpiry)
	apiKey.Spec.ExpiresIn = "48h"
	r, c := newTestReconciler(now, apiKey, secret)

	_, err := r.Reconcile(context.Background(), reconcileRequest())
	require.NoError(t, err)

	var updated v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), reconcileRequest().NamespacedName, &updated))
	require.NotNil(t, updated.Status.Primary)
	require.NotNil(t, updated.Status.Primary.ExpiresAt)
	assertSameTime(t, persistedExpiry, updated.Status.Primary.ExpiresAt.Time)
}

func TestReconcile_BackfillsManagedMetadataWithoutChangingKeyState(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	expiresAt := now.Add(45 * time.Minute)

	tests := []struct {
		name            string
		annotationValue string
		labelValue      string
	}{
		{name: "missing"},
		{name: "wrong", annotationValue: "another-controller", labelValue: "another-controller"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiKey, secret := coherentObjects(now, "sk-current", &expiresAt)
			secret.Annotations = map[string]string{"example.com/note": "keep"}
			secret.Labels = map[string]string{"example.com/team": "keep"}
			if tt.annotationValue != "" {
				secret.Annotations[managedByMetadataKey] = tt.annotationValue
			}
			if tt.labelValue != "" {
				secret.Labels[managedByMetadataKey] = tt.labelValue
			}
			expectedData := secret.DeepCopy().Data
			expectedOwners := append([]metav1.OwnerReference(nil), secret.OwnerReferences...)
			expectedStatus := apiKey.DeepCopy().Status
			r, c := newTestReconciler(now, apiKey, secret)

			_, err := r.Reconcile(context.Background(), reconcileRequest())
			require.NoError(t, err)

			var updatedSecret corev1.Secret
			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), &updatedSecret))
			assert.Equal(t, expectedData, updatedSecret.Data)
			assert.Equal(t, expectedOwners, updatedSecret.OwnerReferences)
			assert.Equal(t, managedByValue, updatedSecret.Annotations[managedByMetadataKey])
			assert.Equal(t, managedByValue, updatedSecret.Labels[managedByMetadataKey])
			assert.Equal(t, "keep", updatedSecret.Annotations["example.com/note"])
			assert.Equal(t, "keep", updatedSecret.Labels["example.com/team"])

			var updatedAPIKey v1alpha1.APIKey
			require.NoError(t, c.Get(context.Background(), reconcileRequest().NamespacedName, &updatedAPIKey))
			assert.Equal(t, expectedStatus.Phase, updatedAPIKey.Status.Phase)
			assert.Equal(t, expectedStatus.SecretRef, updatedAPIKey.Status.SecretRef)
			require.NotNil(t, expectedStatus.Primary)
			require.NotNil(t, updatedAPIKey.Status.Primary)
			assert.Equal(t, expectedStatus.Primary.KeyID, updatedAPIKey.Status.Primary.KeyID)
			require.NotNil(t, expectedStatus.Primary.ExpiresAt)
			require.NotNil(t, updatedAPIKey.Status.Primary.ExpiresAt)
			assertSameTime(t, expectedStatus.Primary.ExpiresAt.Time, updatedAPIKey.Status.Primary.ExpiresAt.Time)
			assert.Equal(t, expectedStatus.Secondary, updatedAPIKey.Status.Secondary)
			assert.Equal(t, expectedStatus.LastRotatedAt, updatedAPIKey.Status.LastRotatedAt)
			assert.Equal(t, expectedStatus.LastSyncedAt, updatedAPIKey.Status.LastSyncedAt)
			require.Len(t, updatedAPIKey.Status.Conditions, len(expectedStatus.Conditions))
			for i := range expectedStatus.Conditions {
				expectedCondition := expectedStatus.Conditions[i]
				actualCondition := updatedAPIKey.Status.Conditions[i]
				assert.Equal(t, expectedCondition.Type, actualCondition.Type)
				assert.Equal(t, expectedCondition.Status, actualCondition.Status)
				assert.Equal(t, expectedCondition.ObservedGeneration, actualCondition.ObservedGeneration)
				assert.Equal(t, expectedCondition.Reason, actualCondition.Reason)
				assert.Equal(t, expectedCondition.Message, actualCondition.Message)
				assertSameTime(t, expectedCondition.LastTransitionTime.Time, actualCondition.LastTransitionTime.Time)
			}
		})
	}
}

func TestUpdateSecretWithRetryDoesNotPersistMetadataWhenMutationFails(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	apiKey, secret := coherentObjects(now, "sk-current", nil)
	secret.Annotations = nil
	secret.Labels = nil
	r, c := newTestReconciler(now, apiKey, secret)

	err := r.updateSecretWithRetry(context.Background(), secret, apiKey, func(candidate *corev1.Secret) (bool, error) {
		candidate.Data[keyutil.SecretKey] = []byte("must-not-persist")
		return true, assert.AnError
	})
	require.ErrorIs(t, err, assert.AnError)

	var unchanged corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), &unchanged))
	assert.Equal(t, "sk-current", string(unchanged.Data[keyutil.SecretKey]))
	assert.Empty(t, unchanged.Annotations)
	assert.Empty(t, unchanged.Labels)
}

func TestReconcile_CoherentSecondaryRemainsAvailableWithinGracePeriod(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	apiKey, secret := coherentObjects(now, "sk-current", nil)
	secondaryExpiry := now.Add(30 * time.Minute)
	apiKey.Status.Secondary = &v1alpha1.KeyStatus{
		KeyID:     operatorKeyID("sk-secondary"),
		ExpiresAt: &metav1.Time{Time: secondaryExpiry},
	}
	secret.Data[keyutil.SecondarySecretKey] = []byte("sk-secondary")
	r, c := newTestReconciler(now, apiKey, secret)

	result, err := r.Reconcile(context.Background(), reconcileRequest())
	require.NoError(t, err)
	assert.Equal(t, 30*time.Minute, result.RequeueAfter)

	var updatedSecret corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), &updatedSecret))
	assert.Equal(t, "sk-current", string(updatedSecret.Data[keyutil.SecretKey]))
	assert.Equal(t, "sk-secondary", string(updatedSecret.Data[keyutil.SecondarySecretKey]))
	var updated v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), reconcileRequest().NamespacedName, &updated))
	require.NotNil(t, updated.Status.Secondary)
	assert.Equal(t, operatorKeyID("sk-secondary"), updated.Status.Secondary.KeyID)
	assertSameTime(t, secondaryExpiry, updated.Status.Secondary.ExpiresAt.Time)
}

func TestReconcile_MissingDeclaredSecretGeneratesReplacement(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	oldExpiry := now.Add(time.Hour)
	apiKey, _ := coherentObjects(now, "sk-missing", &oldExpiry)
	apiKey.Spec.ExpiresIn = "2h"
	r, c := newTestReconciler(now, apiKey)

	_, err := r.Reconcile(context.Background(), reconcileRequest())
	require.NoError(t, err)

	var replacement corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: apiKey.Namespace, Name: keyutil.SecretName}, &replacement))
	assert.NotEmpty(t, replacement.Data[keyutil.SecretKey])
	assert.NotEqual(t, operatorKeyID("sk-missing"), operatorKeyID(string(replacement.Data[keyutil.SecretKey])))
	var updated v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), reconcileRequest().NamespacedName, &updated))
	assert.Equal(t, operatorKeyID(string(replacement.Data[keyutil.SecretKey])), updated.Status.Primary.KeyID)
	require.NotNil(t, updated.Status.Primary.ExpiresAt)
	assertSameTime(t, now.Add(2*time.Hour), updated.Status.Primary.ExpiresAt.Time)
}

func TestReconcile_UnownedDeclaredSecretMarksAPIKeyPendingWithoutMutation(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	apiKey, secret := coherentObjects(now, "sk-current", nil)
	secret.OwnerReferences = nil
	delete(secret.Annotations, managedByMetadataKey)
	delete(secret.Labels, managedByMetadataKey)
	secret.Annotations["example.com/note"] = "keep"
	secret.Labels["example.com/team"] = "keep"
	r, c := newTestReconciler(now, apiKey, secret)

	_, err := r.Reconcile(context.Background(), reconcileRequest())
	require.NoError(t, err)

	var untouched corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), &untouched))
	assert.Equal(t, "sk-current", string(untouched.Data[keyutil.SecretKey]))
	assert.NotContains(t, untouched.Annotations, managedByMetadataKey)
	assert.NotContains(t, untouched.Labels, managedByMetadataKey)
	assert.Equal(t, "keep", untouched.Annotations["example.com/note"])
	assert.Equal(t, "keep", untouched.Labels["example.com/team"])
	var updated v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), reconcileRequest().NamespacedName, &updated))
	assert.Equal(t, v1alpha1.PhasePending, updated.Status.Phase)
	condition := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, eventReasonSecretConflict, condition.Reason)
}

func TestReconcile_PrimaryMismatchGeneratesFreshCoherentPair(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	oldExpiry := now.Add(8 * time.Hour)
	apiKey, secret := coherentObjects(now, "sk-original", &oldExpiry)
	apiKey.Spec.ExpiresIn = "2h"
	secret.Data[keyutil.SecretKey] = []byte("externally-replaced-value")
	secret.Annotations = nil
	secret.Labels = nil
	r, c := newTestReconciler(now, apiKey, secret)

	_, err := r.Reconcile(context.Background(), reconcileRequest())
	require.NoError(t, err)

	var updatedSecret corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: apiKey.Namespace, Name: secret.Name}, &updatedSecret))
	assert.NotEqual(t, "externally-replaced-value", string(updatedSecret.Data[keyutil.SecretKey]))
	assert.NotEmpty(t, updatedSecret.Data[keyutil.SecondarySecretKey])
	assert.Equal(t, managedByValue, updatedSecret.Annotations[managedByMetadataKey])
	assert.Equal(t, managedByValue, updatedSecret.Labels[managedByMetadataKey])

	var updated v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), reconcileRequest().NamespacedName, &updated))
	assert.Equal(t, operatorKeyID(string(updatedSecret.Data[keyutil.SecretKey])), updated.Status.Primary.KeyID)
	require.NotNil(t, updated.Status.Primary.ExpiresAt)
	assertSameTime(t, now.Add(2*time.Hour), updated.Status.Primary.ExpiresAt.Time)
	assert.Equal(t, operatorKeyID(string(updatedSecret.Data[keyutil.SecondarySecretKey])), updated.Status.Secondary.KeyID)
}

func TestReconcile_PartialRotationWriteRekeysAndAcknowledgesSchedule(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	apiKey, secret := coherentObjects(now, "sk-old", nil)
	apiKey.Spec.Rotation = &v1alpha1.RotationSpec{
		RotateAt:    &metav1.Time{Time: now.Add(-time.Minute)},
		GracePeriod: "30m",
	}
	// Simulate Secret-first rotation followed by a status write failure.
	secret.Data[keyutil.SecondarySecretKey] = []byte("sk-old")
	secret.Data[keyutil.SecretKey] = []byte("sk-partial-replacement")
	r, c := newTestReconciler(now, apiKey, secret)

	_, err := r.Reconcile(context.Background(), reconcileRequest())
	require.NoError(t, err)

	var updatedSecret corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: apiKey.Namespace, Name: secret.Name}, &updatedSecret))
	assert.NotEqual(t, "sk-partial-replacement", string(updatedSecret.Data[keyutil.SecretKey]))
	var updated v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), reconcileRequest().NamespacedName, &updated))
	assert.Equal(t, operatorKeyID(string(updatedSecret.Data[keyutil.SecretKey])), updated.Status.Primary.KeyID)
	require.NotNil(t, updated.Status.LastRotatedAt)
	assertSameTime(t, now, updated.Status.LastRotatedAt.Time)

	firstPrimary := string(updatedSecret.Data[keyutil.SecretKey])
	_, err = r.Reconcile(context.Background(), reconcileRequest())
	require.NoError(t, err)
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: apiKey.Namespace, Name: secret.Name}, &updatedSecret))
	assert.Equal(t, firstPrimary, string(updatedSecret.Data[keyutil.SecretKey]), "acknowledged rotation must not run again")
}

func TestReconcile_RotationCapsOutgoingDeadline(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name           string
		oldLifetime    time.Duration
		newExpiresIn   string
		gracePeriod    string
		expectedExpiry time.Time
	}{
		{
			name:           "old primary deadline",
			oldLifetime:    20 * time.Minute,
			newExpiresIn:   "2h",
			gracePeriod:    "1h",
			expectedExpiry: now.Add(20 * time.Minute),
		},
		{
			name:           "replacement primary deadline",
			oldLifetime:    2 * time.Hour,
			newExpiresIn:   "10m",
			gracePeriod:    "1h",
			expectedExpiry: now.Add(10 * time.Minute),
		},
		{
			name:           "grace deadline",
			oldLifetime:    2 * time.Hour,
			newExpiresIn:   "3h",
			gracePeriod:    "5m",
			expectedExpiry: now.Add(5 * time.Minute),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldExpiry := now.Add(tt.oldLifetime)
			apiKey, secret := coherentObjects(now, "sk-old", &oldExpiry)
			apiKey.Spec.ExpiresIn = tt.newExpiresIn
			apiKey.Spec.Rotation = &v1alpha1.RotationSpec{
				RotateAt:    &metav1.Time{Time: now.Add(-time.Minute)},
				GracePeriod: tt.gracePeriod,
			}
			r, c := newTestReconciler(now, apiKey, secret)

			_, err := r.Reconcile(context.Background(), reconcileRequest())
			require.NoError(t, err)

			var updated v1alpha1.APIKey
			require.NoError(t, c.Get(context.Background(), reconcileRequest().NamespacedName, &updated))
			require.NotNil(t, updated.Status.Secondary)
			require.NotNil(t, updated.Status.Secondary.ExpiresAt)
			assertSameTime(t, tt.expectedExpiry, updated.Status.Secondary.ExpiresAt.Time)
			assert.Equal(t, operatorKeyID("sk-old"), updated.Status.Secondary.KeyID)

			var updatedSecret corev1.Secret
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: apiKey.Namespace, Name: secret.Name}, &updatedSecret))
			assert.Equal(t, "sk-old", string(updatedSecret.Data[keyutil.SecondarySecretKey]))
			assert.Equal(t, updated.Status.Primary.KeyID, operatorKeyID(string(updatedSecret.Data[keyutil.SecretKey])))
			assert.True(t, hasManagedSecretMetadata(&updatedSecret))
		})
	}
}

func TestReconcile_SecondaryCleanupWritesSecretBeforeStatus(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	apiKey, secret := coherentObjects(now, "sk-current", nil)
	secret.Data[keyutil.SecondarySecretKey] = []byte("sk-old")
	apiKey.Status.Secondary = &v1alpha1.KeyStatus{
		KeyID:     operatorKeyID("sk-old"),
		ExpiresAt: &metav1.Time{Time: now},
	}

	secretGoneBeforeStatus := false
	c := fake.NewClientBuilder().
		WithScheme(setupScheme()).
		WithObjects(apiKey, secret).
		WithStatusSubresource(&v1alpha1.APIKey{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, delegate client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if subResourceName == "status" {
					updatedAPIKey := obj.(*v1alpha1.APIKey)
					if updatedAPIKey.Status.Secondary == nil {
						var current corev1.Secret
						require.NoError(t, delegate.Get(ctx, types.NamespacedName{Namespace: secret.Namespace, Name: secret.Name}, &current))
						_, present := current.Data[keyutil.SecondarySecretKey]
						secretGoneBeforeStatus = !present
					}
				}
				return delegate.SubResource(subResourceName).Update(ctx, obj, opts...)
			},
		}).
		Build()
	r := &APIKeyReconciler{Client: c, Clock: clocktesting.NewFakeClock(now)}

	_, err := r.Reconcile(context.Background(), reconcileRequest())
	require.NoError(t, err)
	assert.True(t, secretGoneBeforeStatus)

	var updated v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), reconcileRequest().NamespacedName, &updated))
	assert.Nil(t, updated.Status.Secondary)
	var updatedSecret corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), &updatedSecret))
	assert.True(t, hasManagedSecretMetadata(&updatedSecret))
}

func TestReconcile_ExpiryDeletesOwnedSecretWithUIDAndPreservesPrimary(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	apiKey, secret := coherentObjects(now, "sk-current", &now)
	secret.Data[keyutil.SecondarySecretKey] = []byte("sk-old")
	apiKey.Status.Secondary = &v1alpha1.KeyStatus{KeyID: operatorKeyID("sk-old"), ExpiresAt: &metav1.Time{Time: now.Add(time.Hour)}}

	var deletedWithUID types.UID
	c := fake.NewClientBuilder().
		WithScheme(setupScheme()).
		WithObjects(apiKey, secret).
		WithStatusSubresource(&v1alpha1.APIKey{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, delegate client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				options := (&client.DeleteOptions{}).ApplyOptions(opts)
				require.NotNil(t, options.Preconditions)
				require.NotNil(t, options.Preconditions.UID)
				deletedWithUID = *options.Preconditions.UID
				return delegate.Delete(ctx, obj, opts...)
			},
		}).
		Build()
	r := &APIKeyReconciler{Client: c, Clock: clocktesting.NewFakeClock(now)}

	_, err := r.Reconcile(context.Background(), reconcileRequest())
	require.NoError(t, err)
	assert.Equal(t, secret.UID, deletedWithUID)

	var removed corev1.Secret
	err = c.Get(context.Background(), types.NamespacedName{Namespace: secret.Namespace, Name: secret.Name}, &removed)
	assert.True(t, apierrors.IsNotFound(err))

	var updated v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), reconcileRequest().NamespacedName, &updated))
	assert.Equal(t, v1alpha1.PhaseExpired, updated.Status.Phase)
	require.NotNil(t, updated.Status.Primary)
	assert.Equal(t, operatorKeyID("sk-current"), updated.Status.Primary.KeyID)
	require.NotNil(t, updated.Status.Primary.ExpiresAt)
	assertSameTime(t, now, updated.Status.Primary.ExpiresAt.Time)
	assert.Nil(t, updated.Status.Secondary)
	assert.Nil(t, updated.Status.SecretRef)
}

func TestReconcile_ExpiryDoesNotDeleteUnownedReplacement(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	apiKey, secret := coherentObjects(now, "sk-current", &now)
	secret.OwnerReferences = nil
	secret.Data[keyutil.SecretKey] = []byte("replacement-owned-by-someone-else")
	r, c := newTestReconciler(now, apiKey, secret)

	_, err := r.Reconcile(context.Background(), reconcileRequest())
	require.NoError(t, err)

	var untouched corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: secret.Namespace, Name: secret.Name}, &untouched))
	assert.Equal(t, "replacement-owned-by-someone-else", string(untouched.Data[keyutil.SecretKey]))
	var updated v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), reconcileRequest().NamespacedName, &updated))
	assert.Equal(t, v1alpha1.PhaseExpired, updated.Status.Phase)
	assert.Nil(t, updated.Status.SecretRef)
}

func TestReconcile_ExpiryDeleteFailureDoesNotClaimCompletion(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	apiKey, secret := coherentObjects(now, "sk-current", &now)
	c := fake.NewClientBuilder().
		WithScheme(setupScheme()).
		WithObjects(apiKey, secret).
		WithStatusSubresource(&v1alpha1.APIKey{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				return assert.AnError
			},
		}).
		Build()
	r := &APIKeyReconciler{Client: c, Clock: clocktesting.NewFakeClock(now)}

	_, err := r.Reconcile(context.Background(), reconcileRequest())
	require.ErrorIs(t, err, assert.AnError)

	var updated v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), reconcileRequest().NamespacedName, &updated))
	assert.Equal(t, v1alpha1.PhaseActive, updated.Status.Phase)
	require.NotNil(t, updated.Status.SecretRef)
	var retained corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: secret.Namespace, Name: secret.Name}, &retained))
}

func TestReconcile_ExpiredStatusFinishesLegacySecretCleanup(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	expiresAt := now.Add(-time.Minute)
	apiKey, secret := coherentObjects(now, "sk-current", &expiresAt)
	apiKey.Status.Phase = v1alpha1.PhaseExpired
	r, c := newTestReconciler(now, apiKey, secret)

	_, err := r.Reconcile(context.Background(), reconcileRequest())
	require.NoError(t, err)

	var removed corev1.Secret
	err = c.Get(context.Background(), types.NamespacedName{Namespace: secret.Namespace, Name: secret.Name}, &removed)
	assert.True(t, apierrors.IsNotFound(err))
	var updated v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), reconcileRequest().NamespacedName, &updated))
	assert.Equal(t, v1alpha1.PhaseExpired, updated.Status.Phase)
	assert.Nil(t, updated.Status.SecretRef)
	assert.Nil(t, updated.Status.Secondary)
	assert.Equal(t, operatorKeyID("sk-current"), updated.Status.Primary.KeyID)
}

func TestReconcile_RotationStatusConflictUsesLatestSpecOnRecovery(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	oldExpiry := now.Add(4 * time.Hour)
	apiKey, secret := coherentObjects(now, "sk-old", &oldExpiry)
	apiKey.Spec.ExpiresIn = "2h"
	apiKey.Spec.Rotation = &v1alpha1.RotationSpec{
		RotateAt:    &metav1.Time{Time: now.Add(-time.Minute)},
		GracePeriod: "30m",
	}

	conflictInjected := false
	c := fake.NewClientBuilder().
		WithScheme(setupScheme()).
		WithObjects(apiKey, secret).
		WithStatusSubresource(&v1alpha1.APIKey{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, delegate client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if subResourceName == "status" && !conflictInjected {
					conflictInjected = true
					var latest v1alpha1.APIKey
					require.NoError(t, delegate.Get(ctx, reconcileRequest().NamespacedName, &latest))
					latest.Spec.ExpiresIn = "5m"
					latest.Spec.Rotation.RotateAt = &metav1.Time{Time: now.Add(-30 * time.Second)}
					require.NoError(t, delegate.Update(ctx, &latest))
					return apierrors.NewConflict(v1alpha1.GroupVersion.WithResource("apikeys").GroupResource(), obj.GetName(), assert.AnError)
				}
				return delegate.SubResource(subResourceName).Update(ctx, obj, opts...)
			},
		}).
		Build()
	r := &APIKeyReconciler{Client: c, Clock: clocktesting.NewFakeClock(now)}

	_, err := r.Reconcile(context.Background(), reconcileRequest())
	require.True(t, apierrors.IsConflict(err))

	var staleStatus v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), reconcileRequest().NamespacedName, &staleStatus))
	assert.Equal(t, operatorKeyID("sk-old"), staleStatus.Status.Primary.KeyID)
	assert.Nil(t, staleStatus.Status.LastRotatedAt)
	assert.Equal(t, "5m", staleStatus.Spec.ExpiresIn)
	var partiallyRotated corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), &partiallyRotated))
	firstReplacement := string(partiallyRotated.Data[keyutil.SecretKey])
	assert.NotEqual(t, "sk-old", firstReplacement)

	_, err = r.Reconcile(context.Background(), reconcileRequest())
	require.NoError(t, err)

	var updated v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), reconcileRequest().NamespacedName, &updated))
	require.NotNil(t, updated.Status.Primary)
	require.NotNil(t, updated.Status.Primary.ExpiresAt)
	assertSameTime(t, now.Add(5*time.Minute), updated.Status.Primary.ExpiresAt.Time)
	require.NotNil(t, updated.Status.LastRotatedAt)
	assertSameTime(t, now, updated.Status.LastRotatedAt.Time)
	var recovered corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), &recovered))
	assert.NotEqual(t, firstReplacement, string(recovered.Data[keyutil.SecretKey]))
	assert.Equal(t, updated.Status.Primary.KeyID, operatorKeyID(string(recovered.Data[keyutil.SecretKey])))
}

func TestReconcile_ExpiryWaitsForSecretDeletionToFinish(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	apiKey, secret := coherentObjects(now, "sk-current", &now)
	deleteCalls := 0
	c := fake.NewClientBuilder().
		WithScheme(setupScheme()).
		WithObjects(apiKey, secret).
		WithStatusSubresource(&v1alpha1.APIKey{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, delegate client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deleteCalls++
				if deleteCalls == 1 {
					// Simulate an API server accepting deletion while a finalizer keeps
					// the Secret readable.
					return nil
				}
				return delegate.Delete(ctx, obj, opts...)
			},
		}).
		Build()
	r := &APIKeyReconciler{Client: c, Clock: clocktesting.NewFakeClock(now)}

	result, err := r.Reconcile(context.Background(), reconcileRequest())
	require.NoError(t, err)
	assert.Equal(t, time.Second, result.RequeueAfter)
	var pending v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), reconcileRequest().NamespacedName, &pending))
	assert.Equal(t, v1alpha1.PhaseActive, pending.Status.Phase)
	require.NotNil(t, pending.Status.SecretRef)
	var retained corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), &retained))

	result, err = r.Reconcile(context.Background(), reconcileRequest())
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
	assert.Equal(t, 2, deleteCalls)
	var updated v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), reconcileRequest().NamespacedName, &updated))
	assert.Equal(t, v1alpha1.PhaseExpired, updated.Status.Phase)
	assert.Nil(t, updated.Status.SecretRef)
}

func TestReconcile_DeletedUnownedConflictRequeuesSingleton(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	apiKey, secret := coherentObjects(now, "sk-current", nil)
	secret.OwnerReferences = nil
	r, c := newTestReconciler(now, apiKey, secret)

	_, err := r.Reconcile(context.Background(), reconcileRequest())
	require.NoError(t, err)
	require.NoError(t, c.Delete(context.Background(), secret))

	requests := r.secretToAPIKey(context.Background(), secret)
	require.Len(t, requests, 1)
	assert.Equal(t, reconcileRequest(), requests[0])
	unrelated := secret.DeepCopy()
	unrelated.Name = "unrelated"
	assert.Empty(t, r.secretToAPIKey(context.Background(), unrelated))
	_, err = r.Reconcile(context.Background(), requests[0])
	require.NoError(t, err)

	var replacement corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), &replacement))
	assert.True(t, hasExactAPIKeyControllerOwner(&replacement, apiKey))
	var updated v1alpha1.APIKey
	require.NoError(t, c.Get(context.Background(), reconcileRequest().NamespacedName, &updated))
	assert.Equal(t, v1alpha1.PhaseActive, updated.Status.Phase)
}

func TestHasExactAPIKeyControllerOwner(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	apiKey, secret := coherentObjects(now, "sk-current", nil)
	assert.True(t, hasExactAPIKeyControllerOwner(secret, apiKey))

	tests := map[string]func(*v1alpha1.APIKey, *corev1.Secret){
		"API version": func(_ *v1alpha1.APIKey, secret *corev1.Secret) {
			secret.OwnerReferences[0].APIVersion = "kaito.sh/v9"
		},
		"kind": func(_ *v1alpha1.APIKey, secret *corev1.Secret) {
			secret.OwnerReferences[0].Kind = "Other"
		},
		"name": func(_ *v1alpha1.APIKey, secret *corev1.Secret) {
			secret.OwnerReferences[0].Name = "other"
		},
		"owner UID": func(_ *v1alpha1.APIKey, secret *corev1.Secret) {
			secret.OwnerReferences[0].UID = "other"
		},
		"APIKey UID": func(apiKey *v1alpha1.APIKey, _ *corev1.Secret) {
			apiKey.UID = "other"
		},
		"controller flag": func(_ *v1alpha1.APIKey, secret *corev1.Secret) {
			controller := false
			secret.OwnerReferences[0].Controller = &controller
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidateAPIKey := apiKey.DeepCopy()
			candidateSecret := secret.DeepCopy()
			mutate(candidateAPIKey, candidateSecret)
			assert.False(t, hasExactAPIKeyControllerOwner(candidateSecret, candidateAPIKey))
		})
	}
}

func TestGracePeriod(t *testing.T) {
	r := &APIKeyReconciler{}
	apiKey := &v1alpha1.APIKey{Spec: v1alpha1.APIKeySpec{Prefix: "sk-"}}
	assert.Equal(t, 24*time.Hour, v1alpha1.DefaultGracePeriod)
	assert.Equal(t, v1alpha1.DefaultGracePeriod, r.gracePeriod(apiKey))

	apiKey.Spec.Rotation = &v1alpha1.RotationSpec{GracePeriod: "not-a-duration"}
	assert.Equal(t, v1alpha1.DefaultGracePeriod, r.gracePeriod(apiKey))
	apiKey.Spec.Rotation.GracePeriod = "30m"
	assert.Equal(t, 30*time.Minute, r.gracePeriod(apiKey))
}
