package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter, "expected requeue after Secret creation")

	// Verify Secret was created
	var secret corev1.Secret
	err = c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"}, &secret)
	require.NoError(t, err, "expected Secret to be created")

	rawKey := string(secret.Data["apiKey"])
	assert.NotEmpty(t, rawKey, "expected non-empty API key in Secret")

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

	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated)
	assert.Equal(t, v1alpha1.PhaseActive, updated.Status.Phase)
	require.NotNil(t, updated.Status.Current, "expected current to be set")
	assert.NotEmpty(t, updated.Status.Current.KeyID, "expected current.keyID to be set")
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
	require.NoError(t, err)

	var updated v1alpha1.APIKey
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated)
	assert.Equal(t, v1alpha1.PhaseExpired, updated.Status.Phase)
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
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter, "expected requeue when Secret is missing")

	// Verify secretRef was cleared
	var updated v1alpha1.APIKey
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated)
	assert.Nil(t, updated.Status.SecretRef, "expected status.secretRef to be cleared")
}

func TestReconcile_Rotation(t *testing.T) {
	s := setupScheme()
	rawKey := "sk-llmgw-Jx3kLmNpQrStUvWxYzAbCdEfGh0123456789AB"
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
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter, "expected requeue after grace period")

	// Verify Secret has both keys
	var updatedSecret corev1.Secret
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"}, &updatedSecret)
	assert.Equal(t, rawKey, string(updatedSecret.Data["previousApiKey"]), "expected previous key in secret to match original key")
	assert.NotEqual(t, rawKey, string(updatedSecret.Data["apiKey"]), "expected new key to be different from old key")

	// Verify status
	var updated v1alpha1.APIKey
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated)
	assert.Equal(t, v1alpha1.PhaseActive, updated.Status.Phase)
	require.NotNil(t, updated.Status.Previous, "expected status.previous to be set")
	assert.Equal(t, "oldkeyid", updated.Status.Previous.KeyID)
	assert.NotNil(t, updated.Status.LastRotatedAt, "expected status.lastRotatedAt to be set")
	assert.NotEqual(t, "oldkeyid", updated.Status.Current.KeyID, "expected status.current.keyID to change after rotation")

	// Verify spec.rotation.rotateAt was NOT mutated (controller should not write to spec)
	assert.NotNil(t, updated.Spec.Rotation, "expected spec.rotation to be preserved")
	assert.NotNil(t, updated.Spec.Rotation.RotateAt, "expected spec.rotation.rotateAt to be preserved (controller must not mutate spec)")

	// Verify lastRotatedAt >= rotateAt (rotation acknowledged via status)
	assert.False(t, updated.Status.LastRotatedAt.Time.Before(updated.Spec.Rotation.RotateAt.Time),
		"expected status.lastRotatedAt >= spec.rotation.rotateAt")
}

func TestReconcile_RotationWithNewExpiresIn(t *testing.T) {
	s := setupScheme()
	rawKey := "sk-llmgw-Jx3kLmNpQrStUvWxYzAbCdEfGh0123456789AB"
	apiKeyUID := types.UID("rotation-expires-uid")
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
			Name:              "default",
			Namespace:         "team-alpha",
			UID:               apiKeyUID,
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
	require.NoError(t, err)

	var updated v1alpha1.APIKey
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated)

	// ExpiresAt should be ~48h from now (based on rotation time, not creation time)
	require.NotNil(t, updated.Status.Current, "expected current to be set")
	assert.False(t, updated.Status.Current.ExpiresAt.IsZero(), "expected current.expiresAt to be set")
	expiresIn := time.Until(updated.Status.Current.ExpiresAt.Time)
	assert.Greater(t, expiresIn, 47*time.Hour, "expected expiresAt ~48h from now")
	assert.Less(t, expiresIn, 49*time.Hour, "expected expiresAt ~48h from now")
}

func TestReconcile_GracePeriodExpiry(t *testing.T) {
	s := setupScheme()
	rawKey := "sk-llmgw-Jx3kLmNpQrStUvWxYzAbCdEfGh0123456789AB"
	previousKey := "sk-llmgw-OldKeyOldKeyOldKeyOldKeyOldKeyABCD"
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
			"apiKey":         []byte(rawKey),
			"previousApiKey": []byte(previousKey),
		},
	}
	pastTime := metav1.NewTime(time.Now().Add(-1 * time.Hour))
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
	require.NoError(t, err)

	// Verify status.previous was cleared
	var updated v1alpha1.APIKey
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated)
	assert.Nil(t, updated.Status.Previous, "expected status.previous to be cleared after grace period")

	// Verify previousApiKey was removed from Secret
	var updatedSecret corev1.Secret
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"}, &updatedSecret)
	_, hasPrevKey := updatedSecret.Data["previousApiKey"]
	assert.False(t, hasPrevKey, "expected previousApiKey to be removed from Secret")
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
	require.NoError(t, err)

	// Verify the unowned Secret data was NOT overwritten
	var secret corev1.Secret
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"}, &secret)
	assert.Equal(t, "someone-elses-key", string(secret.Data["apiKey"]), "expected unowned Secret data to remain unchanged")

	// Verify APIKey is marked NotReady with SecretConflict
	var updated v1alpha1.APIKey
	c.Get(context.Background(), types.NamespacedName{Namespace: "team-alpha", Name: "default"}, &updated)
	assert.Equal(t, v1alpha1.PhasePending, updated.Status.Phase)
}
