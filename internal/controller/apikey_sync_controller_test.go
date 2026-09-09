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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
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
	require.Equal(t, 1, idx.Size())

	recorder := events.NewFakeRecorder(10)
	syncer := &IndexSyncer{Client: c, KeyIndex: idx, Recorder: recorder}
	_, err := syncer.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"},
	})
	require.NoError(t, err)

	// Index should be cleared for the namespace without emitting an Event.
	assert.Equal(t, 0, idx.Size(), "expected index size 0 after Secret not found")
	requireNoEvent(t, recorder)
}

func TestSyncReconcile_EmptyKey(t *testing.T) {
	s := setupScheme()
	idx := keyindex.New()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llm-api-key",
			Namespace: "team-alpha",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1alpha1.GroupVersion.String(),
				Kind:       "APIKey",
				Name:       "default",
				UID:        types.UID("apikey-uid"),
			}},
		},
		Data: map[string][]byte{
			keyutil.SecretKey: []byte(""),
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()

	// Pre-populate
	idx.Upsert("oldhash", keyindex.TenantRecord{Namespace: "team-alpha", KeyID: "old"})

	recorder := events.NewFakeRecorder(10)
	syncer := &IndexSyncer{Client: c, KeyIndex: idx, Recorder: recorder}
	_, err := syncer.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, idx.Size(), "expected index size 0 for empty key")
	requireEvent(t, recorder, `Warning EmptyAPIKey Managed Secret "llm-api-key" has no primary API key; authorization index was cleared`)
}

func TestSyncReconcile_MissingKeyField(t *testing.T) {
	s := setupScheme()
	idx := keyindex.New()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llm-api-key",
			Namespace: "team-alpha",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1alpha1.GroupVersion.String(),
				Kind:       "APIKey",
				Name:       "default",
				UID:        types.UID("apikey-uid"),
			}},
		},
		Data: map[string][]byte{},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()
	recorder := events.NewFakeRecorder(10)
	syncer := &IndexSyncer{Client: c, KeyIndex: idx, Recorder: recorder}

	_, err := syncer.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"},
	})
	require.NoError(t, err)
	requireEvent(t, recorder, `Warning EmptyAPIKey Managed Secret "llm-api-key" has no primary API key; authorization index was cleared`)
}

func TestSyncReconcile_SyncsPrimaryKey(t *testing.T) {
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

	recorder := events.NewFakeRecorder(10)
	syncer := &IndexSyncer{Client: c, KeyIndex: idx, Recorder: recorder}
	_, err := syncer.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"},
	})
	require.NoError(t, err)
	requireNoEvent(t, recorder)

	expectedHash := keyutil.HashKey(rawKey)
	expectedKeyID := keyutil.KeyIDFromHash(expectedHash)

	rec, ok := idx.Lookup(expectedHash)
	require.True(t, ok, "expected key hash to be in index")
	assert.Equal(t, "team-alpha", rec.Namespace)
	assert.Equal(t, expectedKeyID, rec.KeyID)
	assert.Equal(t, 1, idx.Size())
}

func TestSyncReconcile_SyncsPrimaryAndSecondaryKey(t *testing.T) {
	s := setupScheme()
	idx := keyindex.New()

	rawKey := "sk-primary-key"
	prevKey := "sk-secondary-key"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llm-api-key",
			Namespace: "team-alpha",
		},
		Data: map[string][]byte{
			keyutil.SecretKey:          []byte(rawKey),
			keyutil.SecondarySecretKey: []byte(prevKey),
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()

	recorder := events.NewFakeRecorder(10)
	syncer := &IndexSyncer{Client: c, KeyIndex: idx, Recorder: recorder}
	_, err := syncer.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"},
	})
	require.NoError(t, err)
	requireNoEvent(t, recorder)

	// Both keys should be in the index
	currentHash := keyutil.HashKey(rawKey)
	prevHash := keyutil.HashKey(prevKey)

	_, ok := idx.Lookup(currentHash)
	assert.True(t, ok, "expected primary key hash in index")
	_, ok = idx.Lookup(prevHash)
	assert.True(t, ok, "expected secondary key hash in index")
	assert.Equal(t, 2, idx.Size())
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
	require.NoError(t, err)

	// Stale entry should be gone
	_, ok := idx.Lookup("stale-hash")
	assert.False(t, ok, "expected stale hash to be removed from index")

	// New entry should be present
	newHash := keyutil.HashKey(newKey)
	_, ok = idx.Lookup(newHash)
	assert.True(t, ok, "expected new key hash in index")
	assert.Equal(t, 1, idx.Size())
}

func TestSyncReconcile_EmptySecondaryKeyIgnored(t *testing.T) {
	s := setupScheme()
	idx := keyindex.New()

	rawKey := "sk-only-primary"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llm-api-key",
			Namespace: "team-alpha",
		},
		Data: map[string][]byte{
			keyutil.SecretKey:          []byte(rawKey),
			keyutil.SecondarySecretKey: []byte(""),
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()

	syncer := &IndexSyncer{Client: c, KeyIndex: idx}
	_, err := syncer.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "llm-api-key"},
	})
	require.NoError(t, err)

	// Only the primary key should be indexed (empty secondaryApiKey is ignored)
	assert.Equal(t, 1, idx.Size())
	currentHash := keyutil.HashKey(rawKey)
	_, ok := idx.Lookup(currentHash)
	assert.True(t, ok, "expected primary key hash in index")
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
	require.NoError(t, err)

	// Reconcile second namespace
	_, err = syncer.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-beta", Name: "llm-api-key"},
	})
	require.NoError(t, err)

	assert.Equal(t, 2, idx.Size())

	alphaHash := keyutil.HashKey("sk-alpha-key")
	betaHash := keyutil.HashKey("sk-beta-key")

	rec, ok := idx.Lookup(alphaHash)
	require.True(t, ok, "expected alpha key in index")
	assert.Equal(t, "team-alpha", rec.Namespace)

	rec, ok = idx.Lookup(betaHash)
	require.True(t, ok, "expected beta key in index")
	assert.Equal(t, "team-beta", rec.Namespace)
}

func TestAPIKeyEventSubject(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llm-api-key",
			Namespace: "team-alpha",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1alpha1.GroupVersion.String(),
				Kind:       "APIKey",
				Name:       "default",
				UID:        types.UID("apikey-uid"),
			}},
		},
	}

	subject := apiKeyEventSubject(secret)
	require.NotNil(t, subject)
	assert.Equal(t, v1alpha1.GroupVersion.String(), subject.APIVersion)
	assert.Equal(t, "APIKey", subject.Kind)
	assert.Equal(t, "team-alpha", subject.Namespace)
	assert.Equal(t, "default", subject.Name)
	assert.Equal(t, types.UID("apikey-uid"), subject.UID)
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
			assert.Equal(t, tt.expected, apiKeyOwnerReference(tt.obj) != nil)
		})
	}
}
