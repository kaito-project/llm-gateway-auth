// Package controller provides a IndexSyncer controller that watches Secret resources and
// keeps an in-memory key index up to date. This component is intended to be
// the foundation of an Istio gateway authz plugin.
package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha1 "github.com/kaito-project/llm-gateway-auth/api/v1alpha1"
	"github.com/kaito-project/llm-gateway-auth/internal/keyindex"
	"github.com/kaito-project/llm-gateway-auth/internal/keyutil"
)

// IndexSyncer watches managed Secrets and keeps a keyindex.Index in sync with
// the current set of valid key hashes.
type IndexSyncer struct {
	client.Client
	KeyIndex *keyindex.Index
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

	if prevRaw, ok := secret.Data[keyutil.PreviousSecretKey]; ok && len(prevRaw) > 0 {
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
		for _, ref := range obj.GetOwnerReferences() {
			if ref.APIVersion == v1alpha1.GroupVersion.String() && ref.Kind == "APIKey" {
				return true
			}
		}
		return false
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Secret{}, builder.WithPredicates(managedSecretPredicate)).
		Complete(s)
}
