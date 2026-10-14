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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	v1alpha1 "github.com/kaito-project/llm-gateway-auth/api/v1alpha1"
	"github.com/kaito-project/llm-gateway-auth/internal/keyindex"
	"github.com/kaito-project/llm-gateway-auth/internal/keyutil"
)

const syncTestNamespace = "team-alpha"

func keyID(rawKey string) string {
	return keyutil.KeyIDFromHash(keyutil.HashKey(rawKey))
}

func metadataTime(value time.Time) *metav1.Time {
	result := metav1.NewTime(value)
	return &result
}

func syncFixture(
	namespace string,
	primaryRaw string,
	primaryExpiry *time.Time,
	secondaryRaw string,
	secondaryExpiry *time.Time,
) (*v1alpha1.APIKey, *corev1.Secret) {
	const uid = types.UID("apikey-uid")
	controller := true
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keyutil.SingletonName,
			Namespace: namespace,
			UID:       uid,
		},
		Status: v1alpha1.APIKeyStatus{
			Phase: v1alpha1.PhaseActive,
			Primary: &v1alpha1.KeyStatus{
				KeyID: keyID(primaryRaw),
			},
			SecretRef: &v1alpha1.SecretReference{Name: keyutil.SecretName},
		},
	}
	if primaryExpiry != nil {
		apiKey.Status.Primary.ExpiresAt = metadataTime(*primaryExpiry)
	}
	if secondaryRaw != "" {
		apiKey.Status.Secondary = &v1alpha1.KeyStatus{KeyID: keyID(secondaryRaw)}
		if secondaryExpiry != nil {
			apiKey.Status.Secondary.ExpiresAt = metadataTime(*secondaryExpiry)
		}
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keyutil.SecretName,
			Namespace: namespace,
			UID:       types.UID("secret-uid"),
			Labels: map[string]string{
				managedByMetadataKey: managedByValue,
			},
			Annotations: map[string]string{
				managedByMetadataKey: managedByValue,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1alpha1.GroupVersion.String(),
				Kind:       "APIKey",
				Name:       keyutil.SingletonName,
				UID:        uid,
				Controller: &controller,
			}},
		},
		Data: map[string][]byte{keyutil.SecretKey: []byte(primaryRaw)},
	}
	if secondaryRaw != "" {
		secret.Data[keyutil.SecondarySecretKey] = []byte(secondaryRaw)
	}
	return apiKey, secret
}

func reconcileSyncer(t *testing.T, syncer *IndexSyncer, namespace string) {
	t.Helper()
	_, err := syncer.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: namespace,
		Name:      keyutil.SingletonName,
	}})
	require.NoError(t, err)
}

func TestBuildNamespaceEntries(t *testing.T) {
	primaryExpiry := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
	secondaryExpiry := primaryExpiry.Add(-time.Hour)

	t.Run("non-expiring primary", func(t *testing.T) {
		apiKey, secret := syncFixture(syncTestNamespace, "sk-primary", nil, "", nil)
		entries, err := buildNamespaceEntries(apiKey, secret)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		record := entries[keyutil.HashKey("sk-primary")]
		assert.Equal(t, syncTestNamespace, record.Namespace)
		assert.Equal(t, keyID("sk-primary"), record.KeyID)
		assert.True(t, record.ExpiresAt.IsZero())
	})

	t.Run("finite primary and secondary", func(t *testing.T) {
		apiKey, secret := syncFixture(
			syncTestNamespace,
			"sk-primary",
			&primaryExpiry,
			"sk-secondary",
			&secondaryExpiry,
		)
		entries, err := buildNamespaceEntries(apiKey, secret)
		require.NoError(t, err)
		require.Len(t, entries, 2)
		assert.Equal(t, primaryExpiry, entries[keyutil.HashKey("sk-primary")].ExpiresAt)
		assert.Equal(t, secondaryExpiry, entries[keyutil.HashKey("sk-secondary")].ExpiresAt)
	})

	t.Run("matches across physical slots", func(t *testing.T) {
		apiKey, secret := syncFixture(
			syncTestNamespace,
			"sk-primary",
			&primaryExpiry,
			"sk-secondary",
			&secondaryExpiry,
		)
		secret.Data[keyutil.SecretKey], secret.Data[keyutil.SecondarySecretKey] =
			secret.Data[keyutil.SecondarySecretKey], secret.Data[keyutil.SecretKey]

		entries, err := buildNamespaceEntries(apiKey, secret)
		require.NoError(t, err)
		require.Len(t, entries, 2)
		assert.Equal(t, primaryExpiry, entries[keyutil.HashKey("sk-primary")].ExpiresAt)
		assert.Equal(t, secondaryExpiry, entries[keyutil.HashKey("sk-secondary")].ExpiresAt)
	})

	t.Run("permits stale secondary status after Secret cleanup", func(t *testing.T) {
		apiKey, secret := syncFixture(
			syncTestNamespace,
			"sk-primary",
			&primaryExpiry,
			"sk-secondary",
			&secondaryExpiry,
		)
		delete(secret.Data, keyutil.SecondarySecretKey)

		entries, err := buildNamespaceEntries(apiKey, secret)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		_, found := entries[keyutil.HashKey("sk-primary")]
		assert.True(t, found)
	})

	t.Run("indexes a well-formed past deadline", func(t *testing.T) {
		expiredAt := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
		apiKey, secret := syncFixture(syncTestNamespace, "sk-expired", &expiredAt, "", nil)
		entries, err := buildNamespaceEntries(apiKey, secret)
		require.NoError(t, err)
		assert.Equal(t, expiredAt, entries[keyutil.HashKey("sk-expired")].ExpiresAt)
	})
}

func TestBuildNamespaceEntriesRejectsIncoherentState(t *testing.T) {
	primaryExpiry := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
	secondaryExpiry := primaryExpiry.Add(-time.Hour)

	tests := []struct {
		name   string
		mutate func(*v1alpha1.APIKey, *corev1.Secret)
	}{
		{
			name: "missing managed annotation",
			mutate: func(_ *v1alpha1.APIKey, secret *corev1.Secret) {
				delete(secret.Annotations, managedByMetadataKey)
			},
		},
		{
			name: "wrong managed annotation",
			mutate: func(_ *v1alpha1.APIKey, secret *corev1.Secret) {
				secret.Annotations[managedByMetadataKey] = "wrong"
			},
		},
		{
			name: "missing primary status",
			mutate: func(apiKey *v1alpha1.APIKey, _ *corev1.Secret) {
				apiKey.Status.Primary = nil
			},
		},
		{
			name: "empty primary status key ID",
			mutate: func(apiKey *v1alpha1.APIKey, _ *corev1.Secret) {
				apiKey.Status.Primary.KeyID = ""
			},
		},
		{
			name: "zero primary expiry",
			mutate: func(apiKey *v1alpha1.APIKey, _ *corev1.Secret) {
				apiKey.Status.Primary.ExpiresAt = &metav1.Time{}
			},
		},
		{
			name: "missing secondary expiry",
			mutate: func(apiKey *v1alpha1.APIKey, _ *corev1.Secret) {
				apiKey.Status.Secondary.ExpiresAt = nil
			},
		},
		{
			name: "zero secondary expiry",
			mutate: func(apiKey *v1alpha1.APIKey, _ *corev1.Secret) {
				apiKey.Status.Secondary.ExpiresAt = &metav1.Time{}
			},
		},
		{
			name: "duplicate status key IDs",
			mutate: func(apiKey *v1alpha1.APIKey, _ *corev1.Secret) {
				apiKey.Status.Secondary.KeyID = apiKey.Status.Primary.KeyID
			},
		},
		{
			name: "Secret-new status-old",
			mutate: func(_ *v1alpha1.APIKey, secret *corev1.Secret) {
				secret.Data[keyutil.SecretKey] = []byte("sk-replacement")
			},
		},
		{
			name: "status-new Secret-old",
			mutate: func(apiKey *v1alpha1.APIKey, _ *corev1.Secret) {
				apiKey.Status.Primary.KeyID = keyID("sk-replacement")
			},
		},
		{
			name: "one present Secret key is unmatched",
			mutate: func(_ *v1alpha1.APIKey, secret *corev1.Secret) {
				secret.Data[keyutil.SecondarySecretKey] = []byte("sk-unknown")
			},
		},
		{
			name: "primary status is not consumed",
			mutate: func(_ *v1alpha1.APIKey, secret *corev1.Secret) {
				secret.Data[keyutil.SecretKey] = secret.Data[keyutil.SecondarySecretKey]
				delete(secret.Data, keyutil.SecondarySecretKey)
			},
		},
		{
			name: "duplicate Secret key material",
			mutate: func(_ *v1alpha1.APIKey, secret *corev1.Secret) {
				secret.Data[keyutil.SecondarySecretKey] = secret.Data[keyutil.SecretKey]
			},
		},
		{
			name: "distinct Secret hashes have an ambiguous short key ID",
			mutate: func(apiKey *v1alpha1.APIKey, secret *corev1.Secret) {
				const first = "sk-collision-102837"
				const second = "sk-collision-188090"
				require.Equal(t, keyID(first), keyID(second))
				apiKey.Status.Primary.KeyID = keyID(first)
				apiKey.Status.Secondary = nil
				secret.Data[keyutil.SecretKey] = []byte(first)
				secret.Data[keyutil.SecondarySecretKey] = []byte(second)
			},
		},
		{
			name: "missing Secret primary",
			mutate: func(_ *v1alpha1.APIKey, secret *corev1.Secret) {
				delete(secret.Data, keyutil.SecretKey)
			},
		},
		{
			name: "Secret reference mismatch",
			mutate: func(apiKey *v1alpha1.APIKey, _ *corev1.Secret) {
				apiKey.Status.SecretRef.Name = "another-secret"
			},
		},
		{
			name: "missing controller owner",
			mutate: func(_ *v1alpha1.APIKey, secret *corev1.Secret) {
				secret.OwnerReferences[0].Controller = nil
			},
		},
		{
			name: "wrong owner API version",
			mutate: func(_ *v1alpha1.APIKey, secret *corev1.Secret) {
				secret.OwnerReferences[0].APIVersion = "kaito.sh/v2"
			},
		},
		{
			name: "wrong owner kind",
			mutate: func(_ *v1alpha1.APIKey, secret *corev1.Secret) {
				secret.OwnerReferences[0].Kind = "Other"
			},
		},
		{
			name: "wrong owner name",
			mutate: func(_ *v1alpha1.APIKey, secret *corev1.Secret) {
				secret.OwnerReferences[0].Name = "other"
			},
		},
		{
			name: "empty owner UID",
			mutate: func(_ *v1alpha1.APIKey, secret *corev1.Secret) {
				secret.OwnerReferences[0].UID = ""
			},
		},
		{
			name: "owner UID mismatch",
			mutate: func(_ *v1alpha1.APIKey, secret *corev1.Secret) {
				secret.OwnerReferences[0].UID = "another-uid"
			},
		},
		{
			name: "Secret is deleting",
			mutate: func(_ *v1alpha1.APIKey, secret *corev1.Secret) {
				now := metav1.Now()
				secret.DeletionTimestamp = &now
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiKey, secret := syncFixture(
				syncTestNamespace,
				"sk-primary",
				&primaryExpiry,
				"sk-secondary",
				&secondaryExpiry,
			)
			tt.mutate(apiKey, secret)
			entries, err := buildNamespaceEntries(apiKey, secret)
			require.Error(t, err)
			assert.Nil(t, entries)
		})
	}
}

func TestSyncReconcilePublishesAndReplacesNamespaceAtomically(t *testing.T) {
	primaryExpiry := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
	secondaryExpiry := primaryExpiry.Add(-time.Hour)
	apiKey, secret := syncFixture(
		syncTestNamespace,
		"sk-primary",
		&primaryExpiry,
		"sk-secondary",
		&secondaryExpiry,
	)
	c := fake.NewClientBuilder().WithScheme(setupScheme()).WithObjects(apiKey, secret).Build()
	idx := keyindex.New()
	idx.Upsert("stale-hash", keyindex.TenantRecord{Namespace: syncTestNamespace, KeyID: "stale"})
	syncer := &IndexSyncer{Client: c, APIReader: c, KeyIndex: idx}

	reconcileSyncer(t, syncer, syncTestNamespace)
	assert.Equal(t, 2, idx.Size())
	_, found := idx.Lookup("stale-hash")
	assert.False(t, found)
	assert.True(t, primaryExpiry.Equal(mustLookupRecord(t, idx, "sk-primary").ExpiresAt))
	assert.True(t, secondaryExpiry.Equal(mustLookupRecord(t, idx, "sk-secondary").ExpiresAt))

	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), secret))
	secret.Data[keyutil.SecondarySecretKey] = []byte("sk-unmatched")
	require.NoError(t, c.Update(context.Background(), secret))
	reconcileSyncer(t, syncer, syncTestNamespace)
	assert.Zero(t, idx.Size(), "one unmatched present key must clear the whole namespace")
}

func TestSyncReconcileRespondsToStatusOnlyChanges(t *testing.T) {
	firstExpiry := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
	secondExpiry := firstExpiry.Add(24 * time.Hour)
	apiKey, secret := syncFixture(syncTestNamespace, "sk-primary", &firstExpiry, "", nil)
	c := fake.NewClientBuilder().WithScheme(setupScheme()).WithObjects(apiKey, secret).Build()
	idx := keyindex.New()
	syncer := &IndexSyncer{Client: c, APIReader: c, KeyIndex: idx}

	reconcileSyncer(t, syncer, syncTestNamespace)
	assert.True(t, firstExpiry.Equal(mustLookupRecord(t, idx, "sk-primary").ExpiresAt))

	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(apiKey), apiKey))
	apiKey.Status.Primary.ExpiresAt = metadataTime(secondExpiry)
	require.NoError(t, c.Update(context.Background(), apiKey))
	reconcileSyncer(t, syncer, syncTestNamespace)
	assert.True(t, secondExpiry.Equal(mustLookupRecord(t, idx, "sk-primary").ExpiresAt))
}

func TestSyncReconcileUsesMetadataCacheAndDirectPayloadReader(t *testing.T) {
	apiKey, secret := syncFixture(syncTestNamespace, "sk-primary", nil, "", nil)
	cachedAPIKeyGets := 0
	cachedMetadataGets := 0
	cachedSecretGets := 0
	cachedClient := fake.NewClientBuilder().
		WithScheme(setupScheme()).
		WithObjects(apiKey, secret).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, delegate client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				switch obj.(type) {
				case *v1alpha1.APIKey:
					cachedAPIKeyGets++
				case *metav1.PartialObjectMetadata:
					cachedMetadataGets++
				case *corev1.Secret:
					cachedSecretGets++
				}
				return delegate.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	directMetadataGets := 0
	directSecretGets := 0
	directReader := fake.NewClientBuilder().
		WithScheme(setupScheme()).
		WithObjects(secret).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, delegate client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				switch obj.(type) {
				case *metav1.PartialObjectMetadata:
					directMetadataGets++
				case *corev1.Secret:
					directSecretGets++
				}
				return delegate.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	idx := keyindex.New()
	syncer := &IndexSyncer{Client: cachedClient, APIReader: directReader, KeyIndex: idx}
	reconcileSyncer(t, syncer, syncTestNamespace)

	assert.Equal(t, 1, cachedAPIKeyGets)
	assert.Equal(t, 1, cachedMetadataGets)
	assert.Zero(t, cachedSecretGets, "cached client must never receive a Secret payload read")
	assert.Zero(t, directMetadataGets, "direct reader is reserved for Secret payload reads")
	assert.Equal(t, 1, directSecretGets)
	assert.Equal(t, 1, idx.Size())
}

func TestSyncReconcileRequiresDirectPayloadReader(t *testing.T) {
	apiKey, secret := syncFixture(syncTestNamespace, "sk-primary", nil, "", nil)
	cachedSecretGets := 0
	c := fake.NewClientBuilder().
		WithScheme(setupScheme()).
		WithObjects(apiKey, secret).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, delegate client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					cachedSecretGets++
				}
				return delegate.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	idx := keyindex.New()
	idx.Upsert("stale-hash", keyindex.TenantRecord{Namespace: syncTestNamespace, KeyID: "stale"})
	syncer := &IndexSyncer{Client: c, KeyIndex: idx}

	_, err := syncer.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: syncTestNamespace,
		Name:      keyutil.SingletonName,
	}})
	require.ErrorContains(t, err, "APIReader is required")
	assert.Zero(t, idx.Size())
	assert.Zero(t, cachedSecretGets, "missing APIReader must not fall back to the cached client")
}

func TestSyncReconcileRequiresExactManagedAnnotationBeforeFullSecretRead(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
	}{
		{name: "missing"},
		{name: "empty", annotations: map[string]string{managedByMetadataKey: ""}},
		{name: "wrong", annotations: map[string]string{managedByMetadataKey: "other"}},
		{name: "wrong case", annotations: map[string]string{managedByMetadataKey: "LLM-Gateway-Auth-Service"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiKey, secret := syncFixture(syncTestNamespace, "sk-primary", nil, "", nil)
			secret.Annotations = tt.annotations
			metadataGets := 0
			fullSecretGets := 0
			c := fake.NewClientBuilder().
				WithScheme(setupScheme()).
				WithObjects(apiKey, secret).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, delegate client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						switch obj.(type) {
						case *metav1.PartialObjectMetadata:
							metadataGets++
						case *corev1.Secret:
							fullSecretGets++
						}
						return delegate.Get(ctx, key, obj, opts...)
					},
				}).
				Build()
			idx := keyindex.New()
			idx.Upsert("stale-hash", keyindex.TenantRecord{Namespace: syncTestNamespace, KeyID: "stale"})
			syncer := &IndexSyncer{Client: c, APIReader: c, KeyIndex: idx}

			reconcileSyncer(t, syncer, syncTestNamespace)
			assert.Zero(t, idx.Size())
			assert.Equal(t, 1, metadataGets)
			assert.Zero(t, fullSecretGets, "Secret payload must not be fetched before the annotation gate passes")
		})
	}
}

func TestSyncReconcileRequiresExactOwnerBeforeFullSecretRead(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*corev1.Secret)
	}{
		{
			name: "missing controller owner",
			mutate: func(secret *corev1.Secret) {
				secret.OwnerReferences = nil
			},
		},
		{
			name: "mismatched controller UID",
			mutate: func(secret *corev1.Secret) {
				secret.OwnerReferences[0].UID = types.UID("other-apikey-uid")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiKey, secret := syncFixture(syncTestNamespace, "sk-primary", nil, "", nil)
			tt.mutate(secret)
			fullSecretGets := 0
			c := fake.NewClientBuilder().
				WithScheme(setupScheme()).
				WithObjects(apiKey, secret).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, delegate client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*corev1.Secret); ok {
							fullSecretGets++
						}
						return delegate.Get(ctx, key, obj, opts...)
					},
				}).
				Build()
			idx := keyindex.New()
			idx.Upsert("stale-hash", keyindex.TenantRecord{Namespace: syncTestNamespace, KeyID: "stale"})
			syncer := &IndexSyncer{Client: c, APIReader: c, KeyIndex: idx}

			reconcileSyncer(t, syncer, syncTestNamespace)
			assert.Zero(t, idx.Size())
			assert.Zero(t, fullSecretGets, "Secret payload must not be fetched before the owner gate passes")
		})
	}
}

func TestSyncReconcileExactAnnotationDoesNotRequireManagedLabel(t *testing.T) {
	apiKey, secret := syncFixture(syncTestNamespace, "sk-primary", nil, "", nil)
	secret.Labels = nil
	c := fake.NewClientBuilder().WithScheme(setupScheme()).WithObjects(apiKey, secret).Build()
	idx := keyindex.New()
	syncer := &IndexSyncer{Client: c, APIReader: c, KeyIndex: idx}

	reconcileSyncer(t, syncer, syncTestNamespace)
	assert.Equal(t, 1, idx.Size())
	_, found := idx.Lookup(keyutil.HashKey("sk-primary"))
	assert.True(t, found)
}

func TestSyncReconcileRejectsMetadataChangesBetweenMetadataAndFullReads(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*corev1.Secret)
	}{
		{
			name: "annotation removed",
			mutate: func(secret *corev1.Secret) {
				delete(secret.Annotations, managedByMetadataKey)
			},
		},
		{
			name: "owner changed",
			mutate: func(secret *corev1.Secret) {
				secret.OwnerReferences[0].UID = types.UID("other-apikey-uid")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiKey, secret := syncFixture(syncTestNamespace, "sk-primary", nil, "", nil)
			metadataGets := 0
			fullSecretGets := 0
			c := fake.NewClientBuilder().
				WithScheme(setupScheme()).
				WithObjects(apiKey, secret).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, delegate client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*metav1.PartialObjectMetadata); ok {
							metadataGets++
							return delegate.Get(ctx, key, obj, opts...)
						}
						if fullSecret, ok := obj.(*corev1.Secret); ok {
							fullSecretGets++
							if err := delegate.Get(ctx, key, obj, opts...); err != nil {
								return err
							}
							tt.mutate(fullSecret)
							return nil
						}
						return delegate.Get(ctx, key, obj, opts...)
					},
				}).
				Build()
			idx := keyindex.New()
			idx.Upsert("stale-hash", keyindex.TenantRecord{Namespace: syncTestNamespace, KeyID: "stale"})
			syncer := &IndexSyncer{Client: c, APIReader: c, KeyIndex: idx}

			reconcileSyncer(t, syncer, syncTestNamespace)
			assert.Zero(t, idx.Size())
			assert.Equal(t, 1, metadataGets)
			assert.Equal(t, 1, fullSecretGets)
		})
	}
}

func TestManagedSecretWatchPredicate(t *testing.T) {
	object := func(annotationValue string) *metav1.PartialObjectMetadata {
		obj := &metav1.PartialObjectMetadata{}
		if annotationValue != "" {
			obj.Annotations = map[string]string{managedByMetadataKey: annotationValue}
		}
		return obj
	}
	exact := object(managedByValue)
	missing := object("")
	wrong := object("wrong")
	predicate := managedSecretWatchPredicate()

	assert.True(t, predicate.Create(event.CreateEvent{Object: exact}))
	assert.False(t, predicate.Create(event.CreateEvent{Object: missing}))
	assert.True(t, predicate.Delete(event.DeleteEvent{Object: exact}))
	assert.False(t, predicate.Delete(event.DeleteEvent{Object: wrong}))
	assert.True(t, predicate.Generic(event.GenericEvent{Object: exact}))
	assert.False(t, predicate.Generic(event.GenericEvent{Object: missing}))

	updates := []struct {
		name     string
		old, new client.Object
		expected bool
	}{
		{name: "exact to exact", old: exact, new: exact, expected: true},
		{name: "exact to missing", old: exact, new: missing, expected: true},
		{name: "exact to wrong", old: exact, new: wrong, expected: true},
		{name: "missing to exact", old: missing, new: exact, expected: true},
		{name: "wrong to exact", old: wrong, new: exact, expected: true},
		{name: "missing to wrong", old: missing, new: wrong},
		{name: "wrong to missing", old: wrong, new: missing},
	}
	for _, tt := range updates {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, predicate.Update(event.UpdateEvent{ObjectOld: tt.old, ObjectNew: tt.new}))
		})
	}
}

func TestSyncReconcileClearsInvalidLifecycleStates(t *testing.T) {
	apiKey, secret := syncFixture(syncTestNamespace, "sk-primary", nil, "", nil)

	tests := []struct {
		name    string
		objects []client.Object
	}{
		{name: "APIKey missing", objects: []client.Object{secret}},
		{
			name: "APIKey is deleting",
			objects: func() []client.Object {
				copy := apiKey.DeepCopy()
				now := metav1.Now()
				copy.DeletionTimestamp = &now
				copy.Finalizers = []string{"test-finalizer"}
				return []client.Object{copy, secret}
			}(),
		},
		{
			name: "APIKey is not Active",
			objects: func() []client.Object {
				copy := apiKey.DeepCopy()
				copy.Status.Phase = v1alpha1.PhaseExpired
				return []client.Object{copy, secret}
			}(),
		},
		{
			name: "Secret reference missing",
			objects: func() []client.Object {
				copy := apiKey.DeepCopy()
				copy.Status.SecretRef = nil
				return []client.Object{copy, secret}
			}(),
		},
		{name: "declared Secret missing", objects: []client.Object{apiKey}},
		{
			name: "declared Secret incoherent",
			objects: func() []client.Object {
				copy := secret.DeepCopy()
				copy.Data[keyutil.SecretKey] = []byte("sk-unknown")
				return []client.Object{apiKey, copy}
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(setupScheme()).WithObjects(tt.objects...).Build()
			idx := keyindex.New()
			idx.Upsert("old-hash", keyindex.TenantRecord{Namespace: syncTestNamespace, KeyID: "old"})
			recorder := events.NewFakeRecorder(10)
			syncer := &IndexSyncer{Client: c, APIReader: c, KeyIndex: idx, Recorder: recorder}

			reconcileSyncer(t, syncer, syncTestNamespace)
			assert.Zero(t, idx.Size())
		})
	}
}

func TestSyncReconcileSecretReadErrorClearsStaleNamespace(t *testing.T) {
	apiKey, secret := syncFixture(syncTestNamespace, "sk-primary", nil, "", nil)
	c := fake.NewClientBuilder().
		WithScheme(setupScheme()).
		WithObjects(apiKey, secret).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, delegate client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					return assert.AnError
				}
				return delegate.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	idx := keyindex.New()
	idx.Upsert("stale-hash", keyindex.TenantRecord{Namespace: syncTestNamespace, KeyID: "stale"})
	syncer := &IndexSyncer{Client: c, APIReader: c, KeyIndex: idx}

	_, err := syncer.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: syncTestNamespace,
		Name:      keyutil.SingletonName,
	}})
	require.ErrorIs(t, err, assert.AnError)
	assert.Zero(t, idx.Size())
}

func TestSyncReconcileNamespaceIsolationAndRecovery(t *testing.T) {
	alphaAPIKey, alphaSecret := syncFixture("team-alpha", "sk-alpha", nil, "", nil)
	betaAPIKey, betaSecret := syncFixture("team-beta", "sk-beta", nil, "", nil)
	betaAPIKey.UID = "beta-apikey-uid"
	betaSecret.OwnerReferences[0].UID = betaAPIKey.UID

	c := fake.NewClientBuilder().WithScheme(setupScheme()).WithObjects(
		alphaAPIKey,
		alphaSecret,
		betaAPIKey,
		betaSecret,
	).Build()
	idx := keyindex.New()
	syncer := &IndexSyncer{Client: c, APIReader: c, KeyIndex: idx}

	reconcileSyncer(t, syncer, "team-alpha")
	reconcileSyncer(t, syncer, "team-beta")
	assert.Equal(t, 2, idx.Size())

	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(alphaSecret), alphaSecret))
	alphaSecret.Data[keyutil.SecretKey] = []byte("sk-incoherent")
	require.NoError(t, c.Update(context.Background(), alphaSecret))
	reconcileSyncer(t, syncer, "team-alpha")
	assert.Equal(t, 1, idx.Size())
	_, alphaFound := idx.Lookup(keyutil.HashKey("sk-alpha"))
	assert.False(t, alphaFound)
	_, betaFound := idx.Lookup(keyutil.HashKey("sk-beta"))
	assert.True(t, betaFound)

	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(alphaSecret), alphaSecret))
	alphaSecret.Data[keyutil.SecretKey] = []byte("sk-alpha")
	require.NoError(t, c.Update(context.Background(), alphaSecret))
	reconcileSyncer(t, syncer, "team-alpha")
	assert.Equal(t, 2, idx.Size())
	_, alphaFound = idx.Lookup(keyutil.HashKey("sk-alpha"))
	assert.True(t, alphaFound)
}

func TestSyncReconcileAPIKeyDeletionCannotBeUndoneByLingeringSecret(t *testing.T) {
	apiKey, secret := syncFixture(syncTestNamespace, "sk-primary", nil, "", nil)
	c := fake.NewClientBuilder().WithScheme(setupScheme()).WithObjects(apiKey, secret).Build()
	idx := keyindex.New()
	syncer := &IndexSyncer{Client: c, APIReader: c, KeyIndex: idx}

	reconcileSyncer(t, syncer, syncTestNamespace)
	require.Equal(t, 1, idx.Size())
	require.NoError(t, c.Delete(context.Background(), apiKey))
	reconcileSyncer(t, syncer, syncTestNamespace)
	assert.Zero(t, idx.Size())

	requests := secretToAPIKey(context.Background(), secret)
	require.Len(t, requests, 1)
	_, err := syncer.Reconcile(context.Background(), requests[0])
	require.NoError(t, err)
	assert.Zero(t, idx.Size(), "a lingering Secret event must not restore deleted APIKey state")
}

func TestSecretToAPIKey(t *testing.T) {
	apiKey, secret := syncFixture(syncTestNamespace, "sk-primary", nil, "", nil)
	_ = apiKey

	requests := secretToAPIKey(context.Background(), secret)
	require.Equal(t, []ctrl.Request{{NamespacedName: types.NamespacedName{
		Namespace: syncTestNamespace,
		Name:      keyutil.SingletonName,
	}}}, requests)

	unrelated := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: syncTestNamespace}}
	assert.Empty(t, secretToAPIKey(context.Background(), unrelated))

	secret.OwnerReferences[0].Controller = nil
	assert.Empty(t, secretToAPIKey(context.Background(), secret))
}

func TestSyncReconcileEmitsSafeCoherenceWarning(t *testing.T) {
	apiKey, secret := syncFixture(syncTestNamespace, "sk-primary", nil, "", nil)
	secret.Data[keyutil.SecretKey] = []byte("sensitive-raw-key")
	c := fake.NewClientBuilder().WithScheme(setupScheme()).WithObjects(apiKey, secret).Build()
	recorder := events.NewFakeRecorder(1)
	syncer := &IndexSyncer{Client: c, APIReader: c, KeyIndex: keyindex.New(), Recorder: recorder}

	reconcileSyncer(t, syncer, syncTestNamespace)
	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "Warning InvalidAPIKeyState")
		assert.NotContains(t, event, "sensitive-raw-key")
	case <-time.After(time.Second):
		t.Fatal("expected warning event")
	}
}

func mustLookupRecord(t *testing.T, idx *keyindex.Index, rawKey string) keyindex.TenantRecord {
	t.Helper()
	record, found := idx.Lookup(keyutil.HashKey(rawKey))
	require.True(t, found)
	return record
}
