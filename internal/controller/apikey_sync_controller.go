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

// Package controller provides an IndexSyncer controller that watches APIKey and
// Secret resources and keeps an in-memory key index up to date.
package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/kaito-project/llm-gateway-auth/api/v1alpha1"
	"github.com/kaito-project/llm-gateway-auth/internal/keyindex"
	"github.com/kaito-project/llm-gateway-auth/internal/keyutil"
)

const eventReasonInvalidAPIKeyState = "InvalidAPIKeyState"

// IndexSyncer watches APIKeys and their declared Secrets and keeps a
// keyindex.Index in sync with coherent Active APIKey status.
type IndexSyncer struct {
	client.Client
	APIReader client.Reader
	KeyIndex  *keyindex.Index
	Recorder  events.EventRecorder
}

type statusKey struct {
	keyID     string
	expiresAt time.Time
	matched   bool
}

func (s *IndexSyncer) emitInvalidStateEvent(apiKey *v1alpha1.APIKey, message string) {
	if s.Recorder == nil || apiKey == nil {
		return
	}
	s.Recorder.Eventf(
		apiKey,
		nil,
		corev1.EventTypeWarning,
		eventReasonInvalidAPIKeyState,
		eventReasonInvalidAPIKeyState,
		"%s",
		message,
	)
}

func (s *IndexSyncer) clearInvalidState(apiKey *v1alpha1.APIKey, namespace, message string) {
	s.KeyIndex.RemoveByNamespace(namespace)
	s.emitInvalidStateEvent(apiKey, message)
}

func (s *IndexSyncer) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	apiKeyName := types.NamespacedName{Namespace: req.Namespace, Name: keyutil.SingletonName}

	var apiKey v1alpha1.APIKey
	if err := s.Get(ctx, apiKeyName, &apiKey); err != nil {
		s.KeyIndex.RemoveByNamespace(req.Namespace)
		if apierrors.IsNotFound(err) {
			logger.Info("APIKey removed, cleared index", "namespace", req.Namespace)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !apiKey.DeletionTimestamp.IsZero() {
		s.KeyIndex.RemoveByNamespace(req.Namespace)
		return ctrl.Result{}, nil
	}
	if apiKey.Status.Phase != v1alpha1.PhaseActive {
		s.KeyIndex.RemoveByNamespace(req.Namespace)
		return ctrl.Result{}, nil
	}
	if apiKey.Status.SecretRef == nil || apiKey.Status.SecretRef.Name == "" {
		s.clearInvalidState(&apiKey, req.Namespace, "APIKey status has no declared Secret; authorization index was cleared")
		return ctrl.Result{}, nil
	}

	secretName := types.NamespacedName{Namespace: req.Namespace, Name: apiKey.Status.SecretRef.Name}
	secretMetadata := &metav1.PartialObjectMetadata{}
	secretMetadata.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
	if err := s.Get(ctx, secretName, secretMetadata); err != nil {
		s.clearInvalidState(&apiKey, req.Namespace, "APIKey declared Secret metadata is unavailable; authorization index was cleared")
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !hasManagedSecretAnnotation(secretMetadata) {
		s.clearInvalidState(&apiKey, req.Namespace, "APIKey declared Secret does not have the required managed annotation; authorization index was cleared")
		return ctrl.Result{}, nil
	}
	if !hasExactAPIKeyControllerOwner(secretMetadata, &apiKey) {
		s.clearInvalidState(&apiKey, req.Namespace, "APIKey declared Secret metadata does not have the expected APIKey controller owner; authorization index was cleared")
		return ctrl.Result{}, nil
	}
	if s.APIReader == nil {
		s.clearInvalidState(&apiKey, req.Namespace, "APIKey Secret payload reader is unavailable; authorization index was cleared")
		return ctrl.Result{}, fmt.Errorf("APIReader is required for Secret payload reads")
	}

	var secret corev1.Secret
	if err := s.APIReader.Get(ctx, secretName, &secret); err != nil {
		s.clearInvalidState(&apiKey, req.Namespace, "APIKey declared Secret is unavailable; authorization index was cleared")
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !hasManagedSecretAnnotation(&secret) {
		s.clearInvalidState(&apiKey, req.Namespace, "APIKey declared Secret managed annotation changed while reading it; authorization index was cleared")
		return ctrl.Result{}, nil
	}

	entries, err := buildNamespaceEntries(&apiKey, &secret)
	if err != nil {
		s.clearInvalidState(&apiKey, req.Namespace, fmt.Sprintf("APIKey status and declared Secret are incoherent: %v; authorization index was cleared", err))
		return ctrl.Result{}, nil
	}

	s.KeyIndex.SetNamespaceKeys(req.Namespace, entries)
	logger.Info("Index synced from APIKey status and declared Secret", "namespace", req.Namespace, "keys", len(entries))
	return ctrl.Result{}, nil
}

func buildNamespaceEntries(apiKey *v1alpha1.APIKey, secret *corev1.Secret) (map[string]keyindex.TenantRecord, error) {
	if !hasManagedSecretAnnotation(secret) {
		return nil, fmt.Errorf("declared Secret does not have the required managed annotation")
	}
	if !secret.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("declared Secret is being deleted")
	}
	if apiKey.Status.SecretRef == nil || secret.Name != apiKey.Status.SecretRef.Name || secret.Namespace != apiKey.Namespace {
		return nil, fmt.Errorf("declared Secret reference does not match the observed Secret")
	}

	if !hasExactAPIKeyControllerOwner(secret, apiKey) {
		return nil, fmt.Errorf("declared Secret does not have the expected APIKey controller owner")
	}

	if apiKey.Status.Primary == nil || apiKey.Status.Primary.KeyID == "" {
		return nil, fmt.Errorf("primary status is missing a key ID")
	}
	primaryExpiry, err := statusExpiry(apiKey.Status.Primary, true)
	if err != nil {
		return nil, fmt.Errorf("invalid primary status: %w", err)
	}

	statusKeys := []*statusKey{{
		keyID:     apiKey.Status.Primary.KeyID,
		expiresAt: primaryExpiry,
	}}
	if apiKey.Status.Secondary != nil {
		if apiKey.Status.Secondary.KeyID == "" {
			return nil, fmt.Errorf("secondary status is missing a key ID")
		}
		if apiKey.Status.Secondary.KeyID == apiKey.Status.Primary.KeyID {
			return nil, fmt.Errorf("primary and secondary status have the same key ID")
		}
		secondaryExpiry, err := statusExpiry(apiKey.Status.Secondary, false)
		if err != nil {
			return nil, fmt.Errorf("invalid secondary status: %w", err)
		}
		statusKeys = append(statusKeys, &statusKey{
			keyID:     apiKey.Status.Secondary.KeyID,
			expiresAt: secondaryExpiry,
		})
	}

	primaryRaw := secret.Data[keyutil.SecretKey]
	if len(primaryRaw) == 0 {
		return nil, fmt.Errorf("declared Secret has no primary key")
	}

	secretKeys := [][]byte{primaryRaw}
	if secondaryRaw := secret.Data[keyutil.SecondarySecretKey]; len(secondaryRaw) > 0 {
		secretKeys = append(secretKeys, secondaryRaw)
	}

	entries := make(map[string]keyindex.TenantRecord, len(secretKeys))
	seenKeyIDs := make(map[string]struct{}, len(secretKeys))
	for _, rawKey := range secretKeys {
		hash := keyutil.HashKey(string(rawKey))
		keyID := keyutil.KeyIDFromHash(hash)
		if _, ok := entries[hash]; ok {
			return nil, fmt.Errorf("declared Secret contains duplicate key material")
		}
		if _, ok := seenKeyIDs[keyID]; ok {
			return nil, fmt.Errorf("declared Secret contains ambiguous key IDs")
		}
		seenKeyIDs[keyID] = struct{}{}

		var match *statusKey
		for _, candidate := range statusKeys {
			if candidate.keyID != keyID {
				continue
			}
			if match != nil || candidate.matched {
				return nil, fmt.Errorf("key ID %q has an ambiguous status match", keyID)
			}
			match = candidate
		}
		if match == nil {
			return nil, fmt.Errorf("key ID %q has no status match", keyID)
		}
		match.matched = true
		entries[hash] = keyindex.TenantRecord{
			Namespace: apiKey.Namespace,
			KeyID:     match.keyID,
			ExpiresAt: match.expiresAt,
		}
	}

	if !statusKeys[0].matched {
		return nil, fmt.Errorf("primary status has no Secret key match")
	}
	return entries, nil
}

func statusExpiry(status *v1alpha1.KeyStatus, allowNever bool) (time.Time, error) {
	if status.ExpiresAt == nil {
		if allowNever {
			return time.Time{}, nil
		}
		return time.Time{}, fmt.Errorf("expiry is missing")
	}
	if status.ExpiresAt.IsZero() {
		return time.Time{}, fmt.Errorf("expiry is zero")
	}
	return status.ExpiresAt.Time, nil
}

func secretToAPIKey(_ context.Context, obj client.Object) []reconcile.Request {
	owner := metav1.GetControllerOf(obj)
	if owner == nil || owner.APIVersion != v1alpha1.GroupVersion.String() || owner.Kind != "APIKey" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: obj.GetNamespace(),
		Name:      keyutil.SingletonName,
	}}}
}

func managedSecretWatchPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return e.Object != nil && hasManagedSecretAnnotation(e.Object)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return e.Object != nil && hasManagedSecretAnnotation(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return (e.ObjectOld != nil && hasManagedSecretAnnotation(e.ObjectOld)) ||
				(e.ObjectNew != nil && hasManagedSecretAnnotation(e.ObjectNew))
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return e.Object != nil && hasManagedSecretAnnotation(e.Object)
		},
	}
}

// SetupWithManager registers the IndexSyncer with the Manager.
func (s *IndexSyncer) SetupWithManager(mgr ctrl.Manager) error {
	if s.APIReader == nil {
		s.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.APIKey{}).
		WatchesMetadata(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(secretToAPIKey),
			builder.WithPredicates(managedSecretWatchPredicate()),
		).
		Complete(s)
}
