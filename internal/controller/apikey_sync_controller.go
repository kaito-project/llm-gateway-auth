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

// Package controller provides a IndexSyncer controller that watches Secret resources and
// keeps an in-memory key index up to date. This component is intended to be
// the foundation of an Istio gateway authz plugin.
package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha1 "github.com/kaito-project/llm-gateway-auth/api/v1alpha1"
	"github.com/kaito-project/llm-gateway-auth/internal/keyindex"
	"github.com/kaito-project/llm-gateway-auth/internal/keyutil"
)

const eventReasonEmptyAPIKey = "EmptyAPIKey"

// IndexSyncer watches managed Secrets and keeps a keyindex.Index in sync with
// the current set of valid key hashes.
type IndexSyncer struct {
	client.Client
	KeyIndex *keyindex.Index
	Recorder events.EventRecorder
}

func apiKeyOwnerReference(obj metav1.Object) *metav1.OwnerReference {
	refs := obj.GetOwnerReferences()
	for i := range refs {
		ref := &refs[i]
		if ref.APIVersion == v1alpha1.GroupVersion.String() && ref.Kind == "APIKey" {
			return ref
		}
	}
	return nil
}

func apiKeyEventSubject(secret *corev1.Secret) *v1alpha1.APIKey {
	owner := apiKeyOwnerReference(secret)
	if owner == nil {
		return nil
	}
	return &v1alpha1.APIKey{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.GroupVersion.String(),
			Kind:       "APIKey",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      owner.Name,
			Namespace: secret.Namespace,
			UID:       owner.UID,
		},
	}
}

func (s *IndexSyncer) emitEmptyAPIKeyEvent(secret *corev1.Secret) {
	if s.Recorder == nil {
		return
	}
	apiKey := apiKeyEventSubject(secret)
	if apiKey == nil {
		return
	}
	s.Recorder.Eventf(
		apiKey,
		nil,
		corev1.EventTypeWarning,
		eventReasonEmptyAPIKey,
		eventReasonEmptyAPIKey,
		"Managed Secret %q has no primary API key; authorization index was cleared",
		secret.Name,
	)
}

func (s *IndexSyncer) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var secret corev1.Secret
	if err := s.Get(ctx, req.NamespacedName, &secret); err != nil {
		if errors.IsNotFound(err) {
			s.KeyIndex.RemoveByNamespace(req.Namespace)
			logger.Info("Managed Secret removed, cleared index", "namespace", req.Namespace)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	rawKey := string(secret.Data[keyutil.SecretKey])
	if rawKey == "" {
		s.KeyIndex.RemoveByNamespace(req.Namespace)
		s.emitEmptyAPIKeyEvent(&secret)
		return ctrl.Result{}, nil
	}

	hash := keyutil.HashKey(rawKey)
	keyID := keyutil.KeyIDFromHash(hash)

	entries := map[string]keyindex.TenantRecord{
		hash: {
			Namespace: req.Namespace,
			KeyID:     keyID,
		},
	}

	if prevRaw, ok := secret.Data[keyutil.SecondarySecretKey]; ok && len(prevRaw) > 0 {
		prevHash := keyutil.HashKey(string(prevRaw))
		entries[prevHash] = keyindex.TenantRecord{
			Namespace: req.Namespace,
			KeyID:     keyutil.KeyIDFromHash(prevHash),
		}
	}

	s.KeyIndex.SetNamespaceKeys(req.Namespace, entries)
	logger.Info("Index synced from Secret", "namespace", req.Namespace, "keyID", keyID)
	return ctrl.Result{}, nil
}

// SetupWithManager registers the IndexSyncer with the given manager, watching
// only managed Secret objects.
func (s *IndexSyncer) SetupWithManager(mgr ctrl.Manager) error {
	managedSecretPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return apiKeyOwnerReference(obj) != nil
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Secret{}, builder.WithPredicates(managedSecretPredicate)).
		Complete(s)
}
