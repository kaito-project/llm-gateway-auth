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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/kaito-project/llm-gateway-auth/api/v1alpha1"
	"github.com/kaito-project/llm-gateway-auth/internal/keyindex"
	"github.com/kaito-project/llm-gateway-auth/internal/keyutil"
)

func primaryNeverAnnotations(rawKey string) map[string]string {
	return map[string]string{
		keyutil.PrimaryExpiryAnnotation: keyutil.NeverExpires,
		keyutil.PrimaryHashAnnotation:   keyutil.HashKey(rawKey),
	}
}

func primaryAndSecondaryAnnotations(primaryRaw, secondaryRaw string, primary, secondary time.Time) map[string]string {
	primaryValue, _ := keyutil.FormatExpiry(primary)
	secondaryValue, _ := keyutil.FormatExpiry(secondary)
	return map[string]string{
		keyutil.PrimaryExpiryAnnotation:   primaryValue,
		keyutil.SecondaryExpiryAnnotation: secondaryValue,
		keyutil.PrimaryHashAnnotation:     keyutil.HashKey(primaryRaw),
		keyutil.SecondaryHashAnnotation:   keyutil.HashKey(secondaryRaw),
	}
}

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
			Name:        "llm-api-key",
			Namespace:   "team-alpha",
			Annotations: primaryNeverAnnotations(rawKey),
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
	primaryExpiry := time.Date(2030, time.January, 2, 0, 0, 0, 0, time.UTC)
	secondaryExpiry := primaryExpiry.Add(-time.Hour)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "llm-api-key",
			Namespace:   "team-alpha",
			Annotations: primaryAndSecondaryAnnotations(rawKey, prevKey, primaryExpiry, secondaryExpiry),
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
			Name:        "llm-api-key",
			Namespace:   "team-alpha",
			Annotations: primaryNeverAnnotations(newKey),
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
			Name:        "llm-api-key",
			Namespace:   "team-alpha",
			Annotations: primaryNeverAnnotations(rawKey),
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
			Name:        "llm-api-key",
			Namespace:   "team-alpha",
			Annotations: primaryNeverAnnotations("sk-alpha-key"),
		},
		Data: map[string][]byte{
			keyutil.SecretKey: []byte("sk-alpha-key"),
		},
	}
	secret2 := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "llm-api-key",
			Namespace:   "team-beta",
			Annotations: primaryNeverAnnotations("sk-beta-key"),
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

func TestSyncReconcile_InvalidMetadataClearsNamespace(t *testing.T) {
	primaryExpiry := time.Date(2030, time.January, 2, 0, 0, 0, 0, time.UTC)
	primaryValue, _ := keyutil.FormatExpiry(primaryExpiry)
	laterSecondaryValue, _ := keyutil.FormatExpiry(primaryExpiry.Add(time.Hour))

	tests := []struct {
		name        string
		data        map[string][]byte
		annotations map[string]string
	}{
		{
			name: "missing primary expiry",
			data: map[string][]byte{keyutil.SecretKey: []byte("sk-primary")},
		},
		{
			name:        "malformed primary expiry",
			data:        map[string][]byte{keyutil.SecretKey: []byte("sk-primary")},
			annotations: map[string]string{keyutil.PrimaryExpiryAnnotation: "tomorrow"},
		},
		{
			name: "missing secondary expiry",
			data: map[string][]byte{
				keyutil.SecretKey:          []byte("sk-primary"),
				keyutil.SecondarySecretKey: []byte("sk-secondary"),
			},
			annotations: map[string]string{keyutil.PrimaryExpiryAnnotation: primaryValue},
		},
		{
			name: "secondary never",
			data: map[string][]byte{
				keyutil.SecretKey:          []byte("sk-primary"),
				keyutil.SecondarySecretKey: []byte("sk-secondary"),
			},
			annotations: map[string]string{
				keyutil.PrimaryExpiryAnnotation:   primaryValue,
				keyutil.SecondaryExpiryAnnotation: keyutil.NeverExpires,
			},
		},
		{
			name: "secondary later than primary",
			data: map[string][]byte{
				keyutil.SecretKey:          []byte("sk-primary"),
				keyutil.SecondarySecretKey: []byte("sk-secondary"),
			},
			annotations: map[string]string{
				keyutil.PrimaryExpiryAnnotation:   primaryValue,
				keyutil.SecondaryExpiryAnnotation: laterSecondaryValue,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := setupScheme()
			idx := keyindex.New()
			idx.Upsert("stale", keyindex.TenantRecord{Namespace: "team-alpha", KeyID: "stale"})
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "llm-api-key",
					Namespace:   "team-alpha",
					Annotations: tt.annotations,
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: v1alpha1.GroupVersion.String(),
						Kind:       "APIKey",
						Name:       "default",
						UID:        types.UID("apikey-uid"),
					}},
				},
				Data: tt.data,
			}
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()
			recorder := events.NewFakeRecorder(10)
			syncer := &IndexSyncer{Client: c, KeyIndex: idx, Recorder: recorder}

			_, err := syncer.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(secret)})
			require.NoError(t, err)
			assert.Zero(t, idx.Size())
			requireEvent(t, recorder, "Warning InvalidKeyMetadata Managed Secret has invalid API key expiry metadata; authorization index was cleared")
		})
	}
}

func TestSyncReconcile_KeyMutationWithoutMetadataUpdateFailsClosed(t *testing.T) {
	primaryExpiry := time.Date(2030, time.January, 2, 0, 0, 0, 0, time.UTC)
	secondaryExpiry := primaryExpiry.Add(-time.Hour)

	for _, mutate := range []struct {
		name string
		key  string
	}{
		{name: "primary changed by rolled-back operator", key: keyutil.SecretKey},
		{name: "secondary changed by rolled-back operator", key: keyutil.SecondarySecretKey},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			primaryRaw := "sk-primary"
			secondaryRaw := "sk-secondary"
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:        keyutil.SecretName,
					Namespace:   "team-alpha",
					Annotations: primaryAndSecondaryAnnotations(primaryRaw, secondaryRaw, primaryExpiry, secondaryExpiry),
				},
				Data: map[string][]byte{
					keyutil.SecretKey:          []byte(primaryRaw),
					keyutil.SecondarySecretKey: []byte(secondaryRaw),
				},
			}
			secret.Data[mutate.key] = []byte("sk-mutated-without-metadata")

			idx := keyindex.New()
			idx.Upsert("stale", keyindex.TenantRecord{Namespace: secret.Namespace, KeyID: "stale"})
			c := fake.NewClientBuilder().WithScheme(setupScheme()).WithObjects(secret).Build()
			syncer := &IndexSyncer{Client: c, KeyIndex: idx}

			_, err := syncer.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(secret)})
			require.NoError(t, err)
			assert.Zero(t, idx.Size())
		})
	}
}

func TestSyncReconcile_StaleSecondaryMetadataWithoutKeyKeepsPrimary(t *testing.T) {
	primaryRaw := "sk-primary"
	secondaryExpiry := time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC)
	secondaryValue, err := keyutil.FormatExpiry(secondaryExpiry)
	require.NoError(t, err)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keyutil.SecretName,
			Namespace: "team-alpha",
			Annotations: map[string]string{
				keyutil.PrimaryExpiryAnnotation:   keyutil.NeverExpires,
				keyutil.PrimaryHashAnnotation:     keyutil.HashKey(primaryRaw),
				keyutil.SecondaryExpiryAnnotation: secondaryValue,
				keyutil.SecondaryHashAnnotation:   keyutil.HashKey("sk-removed-secondary"),
			},
		},
		Data: map[string][]byte{keyutil.SecretKey: []byte(primaryRaw)},
	}
	idx := keyindex.New()
	c := fake.NewClientBuilder().WithScheme(setupScheme()).WithObjects(secret).Build()
	syncer := &IndexSyncer{Client: c, KeyIndex: idx}

	_, err = syncer.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(secret)})
	require.NoError(t, err)
	_, found := idx.Lookup(keyutil.HashKey(primaryRaw))
	assert.True(t, found)
	assert.Equal(t, 1, idx.Size())
}

func TestSyncReconcile_ExpiredRecordAndRelistSafety(t *testing.T) {
	s := setupScheme()
	idx := keyindex.New()
	expiredAt := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	expiredValue, err := keyutil.FormatExpiry(expiredAt)
	require.NoError(t, err)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llm-api-key",
			Namespace: "team-alpha",
			Annotations: map[string]string{
				keyutil.PrimaryExpiryAnnotation: expiredValue,
				keyutil.PrimaryHashAnnotation:   keyutil.HashKey("sk-expired"),
			},
		},
		Data: map[string][]byte{keyutil.SecretKey: []byte("sk-expired")},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()
	syncer := &IndexSyncer{Client: c, KeyIndex: idx}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(secret)}

	_, err = syncer.Reconcile(context.Background(), request)
	require.NoError(t, err)
	record, ok := idx.Lookup(keyutil.HashKey("sk-expired"))
	require.True(t, ok, "expired records may remain indexed for request-time denial")
	assert.Equal(t, expiredAt, record.ExpiresAt)

	require.NoError(t, c.Get(context.Background(), request.NamespacedName, secret))
	secret.Annotations[keyutil.PrimaryExpiryAnnotation] = "invalid"
	require.NoError(t, c.Update(context.Background(), secret))
	_, err = syncer.Reconcile(context.Background(), request)
	require.NoError(t, err)
	assert.Zero(t, idx.Size(), "invalid relist must atomically remove stale records")

	require.NoError(t, c.Get(context.Background(), request.NamespacedName, secret))
	secret.Annotations[keyutil.PrimaryExpiryAnnotation] = expiredValue
	require.NoError(t, c.Update(context.Background(), secret))
	_, err = syncer.Reconcile(context.Background(), request)
	require.NoError(t, err)
	_, ok = idx.Lookup(keyutil.HashKey("sk-expired"))
	assert.True(t, ok, "a later valid relist should restore the record")
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
