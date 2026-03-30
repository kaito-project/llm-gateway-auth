package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/kaito-project/llm-gateway-auth/api/v1alpha1"
	"github.com/kaito-project/llm-gateway-auth/internal/keyindex"
	"github.com/kaito-project/llm-gateway-auth/internal/keyutil"
)

func TestSyncReconcile_SecretNotFound(t *testing.T) {
	s := setupScheme()
	idx := keyindex.New()
	c := fake.NewClientBuilder().WithScheme(s).Build()

	// Pre-populate the index for the namespace
	idx.Upsert("somehash", keyindex.TenantRecord{Namespace: "team-alpha", KeyID: "abcd1234"})
	if idx.Size() != 1 {
		t.Fatalf("expected index size 1, got %d", idx.Size())
	}

	syncer := &IndexSyncer{Client: c, KeyIndex: idx}
	_, err := syncer.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Index should be cleared for the namespace
	if idx.Size() != 0 {
		t.Errorf("expected index size 0 after Secret not found, got %d", idx.Size())
	}
}

func TestSyncReconcile_EmptyKey(t *testing.T) {
	s := setupScheme()
	idx := keyindex.New()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llm-api-key",
			Namespace: "team-alpha",
		},
		Data: map[string][]byte{
			keyutil.SecretKey: []byte(""),
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()

	// Pre-populate
	idx.Upsert("oldhash", keyindex.TenantRecord{Namespace: "team-alpha", KeyID: "old"})

	syncer := &IndexSyncer{Client: c, KeyIndex: idx}
	_, err := syncer.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if idx.Size() != 0 {
		t.Errorf("expected index size 0 for empty key, got %d", idx.Size())
	}
}

func TestSyncReconcile_SyncsCurrentKey(t *testing.T) {
	s := setupScheme()
	idx := keyindex.New()

	rawKey := "sk-test-key-12345"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llm-api-key",
			Namespace: "team-alpha",
		},
		Data: map[string][]byte{
			keyutil.SecretKey: []byte(rawKey),
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()

	syncer := &IndexSyncer{Client: c, KeyIndex: idx}
	_, err := syncer.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedHash := keyutil.HashKey(rawKey)
	expectedKeyID := keyutil.KeyIDFromHash(expectedHash)

	rec, ok := idx.Lookup(expectedHash)
	if !ok {
		t.Fatal("expected key hash to be in index")
	}
	if rec.Namespace != "team-alpha" {
		t.Errorf("expected namespace team-alpha, got %s", rec.Namespace)
	}
	if rec.KeyID != expectedKeyID {
		t.Errorf("expected keyID %s, got %s", expectedKeyID, rec.KeyID)
	}
	if idx.Size() != 1 {
		t.Errorf("expected index size 1, got %d", idx.Size())
	}
}

func TestSyncReconcile_SyncsCurrentAndPreviousKey(t *testing.T) {
	s := setupScheme()
	idx := keyindex.New()

	rawKey := "sk-current-key"
	prevKey := "sk-previous-key"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llm-api-key",
			Namespace: "team-alpha",
		},
		Data: map[string][]byte{
			keyutil.SecretKey:         []byte(rawKey),
			keyutil.PreviousSecretKey: []byte(prevKey),
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()

	syncer := &IndexSyncer{Client: c, KeyIndex: idx}
	_, err := syncer.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Both keys should be in the index
	currentHash := keyutil.HashKey(rawKey)
	prevHash := keyutil.HashKey(prevKey)

	if _, ok := idx.Lookup(currentHash); !ok {
		t.Error("expected current key hash in index")
	}
	if _, ok := idx.Lookup(prevHash); !ok {
		t.Error("expected previous key hash in index")
	}
	if idx.Size() != 2 {
		t.Errorf("expected index size 2, got %d", idx.Size())
	}
}

func TestSyncReconcile_ReplacesStaleEntries(t *testing.T) {
	s := setupScheme()
	idx := keyindex.New()

	// Simulate a previously synced key
	idx.Upsert("stale-hash", keyindex.TenantRecord{Namespace: "team-alpha", KeyID: "stale"})

	newKey := "sk-new-key"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llm-api-key",
			Namespace: "team-alpha",
		},
		Data: map[string][]byte{
			keyutil.SecretKey: []byte(newKey),
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()

	syncer := &IndexSyncer{Client: c, KeyIndex: idx}
	_, err := syncer.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Stale entry should be gone
	if _, ok := idx.Lookup("stale-hash"); ok {
		t.Error("expected stale hash to be removed from index")
	}

	// New entry should be present
	newHash := keyutil.HashKey(newKey)
	if _, ok := idx.Lookup(newHash); !ok {
		t.Error("expected new key hash in index")
	}
	if idx.Size() != 1 {
		t.Errorf("expected index size 1, got %d", idx.Size())
	}
}

func TestSyncReconcile_EmptyPreviousKeyIgnored(t *testing.T) {
	s := setupScheme()
	idx := keyindex.New()

	rawKey := "sk-only-current"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llm-api-key",
			Namespace: "team-alpha",
		},
		Data: map[string][]byte{
			keyutil.SecretKey:         []byte(rawKey),
			keyutil.PreviousSecretKey: []byte(""),
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()

	syncer := &IndexSyncer{Client: c, KeyIndex: idx}
	_, err := syncer.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only the current key should be indexed (empty previousApiKey is ignored)
	if idx.Size() != 1 {
		t.Errorf("expected index size 1, got %d", idx.Size())
	}
	currentHash := keyutil.HashKey(rawKey)
	if _, ok := idx.Lookup(currentHash); !ok {
		t.Error("expected current key hash in index")
	}
}

func TestSyncReconcile_MultipleNamespaces(t *testing.T) {
	s := setupScheme()
	idx := keyindex.New()

	secret1 := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llm-api-key",
			Namespace: "team-alpha",
		},
		Data: map[string][]byte{
			keyutil.SecretKey: []byte("sk-alpha-key"),
		},
	}
	secret2 := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llm-api-key",
			Namespace: "team-beta",
		},
		Data: map[string][]byte{
			keyutil.SecretKey: []byte("sk-beta-key"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(secret1, secret2).Build()
	syncer := &IndexSyncer{Client: c, KeyIndex: idx}

	// Reconcile first namespace
	_, err := syncer.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Reconcile second namespace
	_, err = syncer.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-beta", Name: "llm-api-key"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if idx.Size() != 2 {
		t.Errorf("expected index size 2, got %d", idx.Size())
	}

	alphaHash := keyutil.HashKey("sk-alpha-key")
	betaHash := keyutil.HashKey("sk-beta-key")

	rec, ok := idx.Lookup(alphaHash)
	if !ok {
		t.Fatal("expected alpha key in index")
	}
	if rec.Namespace != "team-alpha" {
		t.Errorf("expected namespace team-alpha, got %s", rec.Namespace)
	}

	rec, ok = idx.Lookup(betaHash)
	if !ok {
		t.Fatal("expected beta key in index")
	}
	if rec.Namespace != "team-beta" {
		t.Errorf("expected namespace team-beta, got %s", rec.Namespace)
	}
}

func TestManagedSecretPredicate(t *testing.T) {
	apiKeyGV := v1alpha1.GroupVersion.String()

	tests := []struct {
		name     string
		obj      metav1.Object
		expected bool
	}{
		{
			name: "owned by APIKey",
			obj: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "llm-api-key",
					Namespace: "team-alpha",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: apiKeyGV,
							Kind:       "APIKey",
							Name:       "default",
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "not owned by APIKey",
			obj: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "other-secret",
					Namespace: "team-alpha",
				},
			},
			expected: false,
		},
		{
			name: "owned by different kind",
			obj: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "other-secret",
					Namespace: "team-alpha",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "apps/v1",
							Kind:       "Deployment",
							Name:       "my-deploy",
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "owned by same API group but different kind",
			obj: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "other-secret",
					Namespace: "team-alpha",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: apiKeyGV,
							Kind:       "SomethingElse",
							Name:       "default",
						},
					},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := false
			for _, ref := range tt.obj.GetOwnerReferences() {
				if ref.APIVersion == apiKeyGV && ref.Kind == "APIKey" {
					result = true
					break
				}
			}
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}
