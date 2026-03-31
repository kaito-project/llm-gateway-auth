package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/kaito-project/llm-gateway-auth/api/v1alpha1"
)

func setupScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	clientgoscheme.AddToScheme(s)
	v1alpha1.AddToScheme(s)
	return s
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReconcile_NotFound(t *testing.T) {
	s := setupScheme()
	c := fake.NewClientBuilder().WithScheme(s).Build()
	r := &APIKeyReconciler{Client: c}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReconcile_GeneratesSecret(t *testing.T) {
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

	// First reconcile: should create Secret and set status.secretRef
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Requeue {
		t.Error("expected requeue after Secret creation")
	}

	// Verify Secret was created
	var secret corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"}, &secret); err != nil {
		t.Fatalf("expected Secret to be created: %v", err)
	}
	rawKey := string(secret.Data["apiKey"])
	if len(rawKey) == 0 {
		t.Fatal("expected non-empty API key in Secret")
	}

	// Verify status.secretRef was set
	var updated v1alpha1.APIKey
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated)
	if updated.Status.SecretRef == nil {
		t.Fatal("expected status.secretRef to be set")
	}
	if updated.Status.SecretRef.Name != "llm-api-key" {
		t.Errorf("expected secretRef.name=llm-api-key, got %s", updated.Status.SecretRef.Name)
	}

	// Second reconcile: should read Secret and update status
	_, err = r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error on second reconcile: %v", err)
	}

	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated)
	if updated.Status.Phase != v1alpha1.PhaseActive {
		t.Errorf("expected phase Active, got %s", updated.Status.Phase)
	}
	if updated.Status.Current == nil || updated.Status.Current.KeyID == "" {
		t.Error("expected current.keyID to be set")
	}
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.APIKey
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated)
	if updated.Status.Phase != v1alpha1.PhaseActive {
		t.Errorf("expected phase Active, got %s", updated.Status.Phase)
	}
}

func TestReconcile_ExpiredKey(t *testing.T) {
	s := setupScheme()
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "default",
			Namespace:         "team-alpha",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * time.Hour)),
		},
		Spec: v1alpha1.APIKeySpec{
			Prefix:    "llmgw",
			ExpiresIn: "1h", // 1 hour after creation, which was 2 hours ago → expired
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey).WithStatusSubresource(apiKey).Build()
	r := &APIKeyReconciler{Client: c}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.APIKey
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated)
	if updated.Status.Phase != v1alpha1.PhaseExpired {
		t.Errorf("expected phase Expired, got %s", updated.Status.Phase)
	}
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
	r := &APIKeyReconciler{Client: c}

	// First reconcile: detect missing Secret, clear secretRef
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Requeue {
		t.Error("expected requeue when Secret is missing")
	}

	// Verify secretRef was cleared
	var updated v1alpha1.APIKey
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated)
	if updated.Status.SecretRef != nil {
		t.Error("expected status.secretRef to be cleared")
	}
}

func TestReconcile_Rotation(t *testing.T) {
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
			Rotation: &v1alpha1.RotationSpec{
				RotateAt: &metav1.Time{Time: time.Now().Add(-1 * time.Minute)},
			},
		},
		Status: v1alpha1.APIKeyStatus{
			Current: &v1alpha1.KeyStatus{
				KeyID: "oldkeyid",
			},
			Phase: v1alpha1.PhaseActive,
			SecretRef: &v1alpha1.SecretReference{
				Name: "llm-api-key",
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey, secret).WithStatusSubresource(apiKey).Build()
	r := &APIKeyReconciler{Client: c}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue after grace period")
	}

	// Verify Secret has both keys
	var updatedSecret corev1.Secret
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"}, &updatedSecret)
	if string(updatedSecret.Data["previousApiKey"]) != rawKey {
		t.Error("expected previous key in secret to match original key")
	}
	newKey := string(updatedSecret.Data["apiKey"])
	if newKey == rawKey {
		t.Error("expected new key to be different from old key")
	}

	// Verify status
	var updated v1alpha1.APIKey
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated)
	if updated.Status.Phase != v1alpha1.PhaseActive {
		t.Errorf("expected phase Active, got %s", updated.Status.Phase)
	}
	if updated.Status.Previous == nil {
		t.Fatal("expected status.previous to be set")
	}
	if updated.Status.Previous.KeyID != "oldkeyid" {
		t.Errorf("expected previous keyID oldkeyid, got %s", updated.Status.Previous.KeyID)
	}
	if updated.Status.LastRotatedAt == nil {
		t.Error("expected status.lastRotatedAt to be set")
	}
	if updated.Status.Current == nil || updated.Status.Current.KeyID == "oldkeyid" {
		t.Error("expected status.current.keyID to change after rotation")
	}

	// Verify spec.rotation.rotateAt was NOT mutated (controller should not write to spec)
	if updated.Spec.Rotation == nil || updated.Spec.Rotation.RotateAt == nil {
		t.Error("expected spec.rotation.rotateAt to be preserved (controller must not mutate spec)")
	}

	// Verify lastRotatedAt >= rotateAt (rotation acknowledged via status)
	if updated.Status.LastRotatedAt.Time.Before(updated.Spec.Rotation.RotateAt.Time) {
		t.Error("expected status.lastRotatedAt >= spec.rotation.rotateAt")
	}

}

func TestReconcile_RotationWithNewExpiresIn(t *testing.T) {
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
			Name:              "default",
			Namespace:         "team-alpha",
			CreationTimestamp: metav1.Now(),
		},
		Spec: v1alpha1.APIKeySpec{
			Prefix:    "sk-",
			ExpiresIn: "48h", // New expiration for the rotated key
			Rotation: &v1alpha1.RotationSpec{
				RotateAt: &metav1.Time{Time: time.Now().Add(-1 * time.Minute)},
			},
		},
		Status: v1alpha1.APIKeyStatus{
			Current: &v1alpha1.KeyStatus{
				KeyID: "oldkeyid",
			},
			Phase: v1alpha1.PhaseActive,
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.APIKey
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated)

	// ExpiresAt should be ~48h from now (based on rotation time, not creation time)
	if updated.Status.Current == nil || updated.Status.Current.ExpiresAt.IsZero() {
		t.Fatal("expected current.expiresAt to be set")
	}
	expiresIn := time.Until(updated.Status.Current.ExpiresAt.Time)
	if expiresIn < 47*time.Hour || expiresIn > 49*time.Hour {
		t.Errorf("expected expiresAt ~48h from now, got %v", expiresIn)
	}
}

func TestReconcile_GracePeriodExpiry(t *testing.T) {
	s := setupScheme()
	rawKey := "sk-llmgw-Jx3kLmNpQrStUvWxYzAbCdEfGh0123456789AB"
	previousKey := "sk-llmgw-OldKeyOldKeyOldKeyOldKeyOldKeyABCD"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llm-api-key",
			Namespace: "team-alpha",
		},
		Data: map[string][]byte{
			"apiKey":         []byte(rawKey),
			"previousApiKey": []byte(previousKey),
		},
	}
	pastTime := metav1.NewTime(time.Now().Add(-1 * time.Hour))
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default",
			Namespace: "team-alpha",
		},
		Spec: v1alpha1.APIKeySpec{
			Prefix: "sk-",
		},
		Status: v1alpha1.APIKeyStatus{
			Current: &v1alpha1.KeyStatus{
				KeyID: "currentid",
			},
			Phase: v1alpha1.PhaseActive,
			SecretRef: &v1alpha1.SecretReference{
				Name: "llm-api-key",
			},
			Previous: &v1alpha1.KeyStatus{
				KeyID:     "prevkeyid",
				ExpiresAt: &metav1.Time{Time: pastTime.Time}, // Grace period already expired
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKey, secret).WithStatusSubresource(apiKey).Build()
	r := &APIKeyReconciler{Client: c}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify status.previous was cleared
	var updated v1alpha1.APIKey
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated)
	if updated.Status.Previous != nil {
		t.Error("expected status.previous to be cleared after grace period")
	}

	// Verify previousApiKey was removed from Secret
	var updatedSecret corev1.Secret
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"}, &updatedSecret)
	if _, ok := updatedSecret.Data["previousApiKey"]; ok {
		t.Error("expected previousApiKey to be removed from Secret")
	}
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
	r := &APIKeyReconciler{Client: c}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the unowned Secret data was NOT overwritten
	var secret corev1.Secret
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"}, &secret)
	if string(secret.Data["apiKey"]) != "someone-elses-key" {
		t.Error("expected unowned Secret data to remain unchanged")
	}

	// Verify APIKey is marked NotReady with SecretConflict
	var updated v1alpha1.APIKey
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated)
	if updated.Status.Phase != v1alpha1.PhasePending {
		t.Errorf("expected phase Pending, got %s", updated.Status.Phase)
	}
}
