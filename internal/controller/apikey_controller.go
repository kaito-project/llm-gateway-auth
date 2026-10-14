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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/kaito-project/llm-gateway-auth/api/v1alpha1"
	"github.com/kaito-project/llm-gateway-auth/internal/keyutil"
)

const (
	finalizerName = "kaito.sh/key-cleanup"

	eventReasonCreated             = "Created"
	eventReasonExpired             = "Expired"
	eventReasonKeyGenerationFailed = "KeyGenerationFailed"
	eventReasonRotated             = "Rotated"
	eventReasonRotationFailed      = "RotationFailed"
	eventReasonSecondaryKeyRemoved = "SecondaryKeyRemoved"
	eventReasonSecretConflict      = "SecretConflict"
	eventReasonSecretCorrupt       = "SecretCorrupt"
	eventReasonSecretMissing       = "SecretMissing"
	eventReasonSecretWriteFailed   = "SecretWriteFailed"

	eventActionCreateSecret           = "CreateSecret"
	eventActionGeneratePrimaryKey     = "GeneratePrimaryKey"
	eventActionGenerateReplacementKey = "GenerateReplacementKey"
	eventActionGenerateSecondaryKey   = "GenerateSecondaryKey"
	eventActionPersistRotationStatus  = "PersistRotationStatus"
	eventActionReadSecret             = "ReadSecret"
	eventActionRotateSecret           = "RotateSecret"
	eventActionUpdateSecret           = "UpdateSecret"
)

// APIKeyReconciler reconciles APIKey objects.
type APIKeyReconciler struct {
	client.Client
	APIReader client.Reader
	Recorder  events.EventRecorder
	Clock     clock.PassiveClock
}

func (r *APIKeyReconciler) now() time.Time {
	if r.Clock == nil {
		return normalizeLifecycleTime(clock.RealClock{}.Now())
	}
	return normalizeLifecycleTime(r.Clock.Now())
}

func normalizeLifecycleTime(value time.Time) time.Time {
	return value.UTC().Truncate(time.Second)
}

func (r *APIKeyReconciler) emitEvent(apiKey *v1alpha1.APIKey, eventType, reason, message string) {
	r.emitEventWithAction(apiKey, eventType, reason, reason, message)
}

func (r *APIKeyReconciler) emitEventWithAction(apiKey *v1alpha1.APIKey, eventType, reason, action, message string) {
	if r.Recorder != nil {
		r.Recorder.Eventf(apiKey, nil, eventType, reason, action, "%s", message)
	}
}

func (r *APIKeyReconciler) emitEventf(apiKey *v1alpha1.APIKey, eventType, reason, messageFmt string, args ...any) {
	r.emitEventfWithAction(apiKey, eventType, reason, reason, messageFmt, args...)
}

func (r *APIKeyReconciler) emitEventfWithAction(apiKey *v1alpha1.APIKey, eventType, reason, action, messageFmt string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(apiKey, nil, eventType, reason, action, messageFmt, args...)
	}
}

func readyConditionHasReason(apiKey *v1alpha1.APIKey, status metav1.ConditionStatus, reason string) bool {
	condition := meta.FindStatusCondition(apiKey.Status.Conditions, "Ready")
	return condition != nil && condition.Status == status && condition.Reason == reason
}

func (r *APIKeyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var apiKey v1alpha1.APIKey
	if err := r.Get(ctx, req.NamespacedName, &apiKey); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !apiKey.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &apiKey)
	}
	if err := r.ensureFinalizer(ctx, &apiKey); err != nil {
		return ctrl.Result{}, err
	}
	now := r.now()
	if apiKey.Status.Phase == v1alpha1.PhaseExpired {
		cleanupComplete, err := r.deleteDeclaredKeySecret(ctx, &apiKey)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !cleanupComplete {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		if apiKey.Status.SecretRef != nil || apiKey.Status.Secondary != nil {
			if err := r.updateStatusWithRetry(ctx, &apiKey, func(obj *v1alpha1.APIKey) {
				obj.Status.SecretRef = nil
				obj.Status.Secondary = nil
			}); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	prefix := apiKey.Spec.Prefix
	if prefix == "" {
		prefix = keyutil.DefaultPrefix
	}

	primaryExpiresAt, hasPrimaryStatus, statusErr := primaryExpiryFromStatus(&apiKey)
	rotationDue := r.isRotationDue(&apiKey, now)
	if statusErr == nil && hasPrimaryStatus && primaryExpiresAt != nil && !now.Before(*primaryExpiresAt) {
		rotateBeforeExpiry := rotationDue && !apiKey.Spec.Rotation.RotateAt.After(*primaryExpiresAt)
		if !rotateBeforeExpiry {
			return r.handleExpiry(ctx, &apiKey, primaryExpiresAt, now)
		}
	}

	secretName := keyutil.SecretName
	if apiKey.Status.SecretRef != nil && apiKey.Status.SecretRef.Name != "" {
		secretName = apiKey.Status.SecretRef.Name
	}

	if apiKey.Status.SecretRef == nil || apiKey.Status.SecretRef.Name == "" {
		expiresAt, err := r.expiryForRekey(&apiKey, now)
		if err != nil {
			return ctrl.Result{}, err
		}
		if expiresAt != nil && !now.Before(*expiresAt) {
			return r.handleExpiry(ctx, &apiKey, expiresAt, now)
		}
		return r.replaceKeySecret(ctx, &apiKey, prefix, secretName, expiresAt, now, rotationDue)
	}

	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{Namespace: apiKey.Namespace, Name: secretName}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		r.emitEventf(&apiKey, corev1.EventTypeWarning, eventReasonSecretMissing, "Secret %q was not found; API key material will be regenerated", secretName)
		expiresAt, expiryErr := replacementPrimaryExpiry(&apiKey, now)
		if expiryErr != nil {
			return ctrl.Result{}, expiryErr
		}
		return r.replaceKeySecret(ctx, &apiKey, prefix, secretName, expiresAt, now, rotationDue)
	}
	if !hasExactAPIKeyControllerOwner(secret, &apiKey) {
		alreadyConflicted := readyConditionHasReason(&apiKey, metav1.ConditionFalse, eventReasonSecretConflict)
		message := fmt.Sprintf("Secret %q is not owned by this APIKey", secretName)
		if !alreadyConflicted {
			if err := r.markNotReady(ctx, &apiKey, eventReasonSecretConflict, message, now); err != nil {
				return ctrl.Result{}, err
			}
			r.emitEvent(&apiKey, corev1.EventTypeWarning, eventReasonSecretConflict, message)
		}
		return ctrl.Result{}, nil
	}

	if !hasManagedSecretMetadata(secret) {
		if err := r.updateSecretWithRetry(ctx, secret, &apiKey, func(*corev1.Secret) (bool, error) {
			return false, nil
		}); err != nil {
			r.emitEventfWithAction(&apiKey, corev1.EventTypeWarning, eventReasonSecretWriteFailed, eventActionUpdateSecret, "Failed to update managed metadata on Secret %q", secretName)
			return ctrl.Result{}, err
		}
	}

	if statusErr != nil || !hasPrimaryStatus || !primaryMatchesSecret(&apiKey, secret) {
		logger.Info("Replacing incoherent API key material", "namespace", apiKey.Namespace, "secret", secretName)
		r.emitEventf(&apiKey, corev1.EventTypeWarning, eventReasonSecretCorrupt, "Secret %q does not match primary key status; API key material will be regenerated", secretName)
		expiresAt, err := replacementPrimaryExpiry(&apiKey, now)
		if err != nil {
			return ctrl.Result{}, err
		}
		return r.replaceKeySecret(ctx, &apiKey, prefix, secretName, expiresAt, now, rotationDue)
	}

	if primaryExpiresAt != nil && !now.Before(*primaryExpiresAt) {
		return r.handleRotation(ctx, &apiKey, secret, prefix, now)
	}
	if rotationDue {
		return r.handleRotation(ctx, &apiKey, secret, prefix, now)
	}

	if err := r.reconcileSecondary(ctx, &apiKey, secret, now); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureActiveStatus(ctx, &apiKey, now); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("APIKey synced", "namespace", apiKey.Namespace, "keyID", primaryKeyID(&apiKey))
	return ctrl.Result{RequeueAfter: r.computeRequeueAfter(&apiKey, primaryExpiresAt, now)}, nil
}

func (r *APIKeyReconciler) updateStatusWithRetry(ctx context.Context, apiKey *v1alpha1.APIKey, mutate func(*v1alpha1.APIKey)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, types.NamespacedName{Namespace: apiKey.Namespace, Name: apiKey.Name}, apiKey); err != nil {
			return err
		}
		mutate(apiKey)
		return r.Status().Update(ctx, apiKey)
	})
}

// updateStatusFromSnapshot intentionally does not retry a conflict. Callers use
// values derived from the observed spec when writing Secret material first; a
// newer spec must force a fresh reconcile rather than receiving stale status.
func (r *APIKeyReconciler) updateStatusFromSnapshot(ctx context.Context, apiKey *v1alpha1.APIKey, mutate func(*v1alpha1.APIKey)) error {
	mutate(apiKey)
	return r.Status().Update(ctx, apiKey)
}

func (r *APIKeyReconciler) updateSecretWithRetry(ctx context.Context, secret *corev1.Secret, apiKey *v1alpha1.APIKey, mutate func(*corev1.Secret) (bool, error)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, types.NamespacedName{Namespace: secret.Namespace, Name: secret.Name}, secret); err != nil {
			return err
		}
		if !hasExactAPIKeyControllerOwner(secret, apiKey) {
			return fmt.Errorf("secret %s/%s is not owned by APIKey %s", secret.Namespace, secret.Name, apiKey.Name)
		}
		changed, err := mutate(secret)
		if err != nil {
			return err
		}
		metadataChanged := ensureManagedSecretMetadata(secret)
		if !changed && !metadataChanged {
			return nil
		}
		return r.Update(ctx, secret)
	})
}

func (r *APIKeyReconciler) markNotReady(ctx context.Context, apiKey *v1alpha1.APIKey, reason, message string, now time.Time) error {
	return r.updateStatusWithRetry(ctx, apiKey, func(obj *v1alpha1.APIKey) {
		meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: metav1.NewTime(now),
		})
		obj.Status.Phase = v1alpha1.PhasePending
	})
}

func (r *APIKeyReconciler) handleDeletion(ctx context.Context, apiKey *v1alpha1.APIKey) (ctrl.Result, error) {
	log.FromContext(ctx).Info("APIKey being deleted", "namespace", apiKey.Namespace, "keyID", primaryKeyID(apiKey))
	if controllerutil.ContainsFinalizer(apiKey, finalizerName) {
		controllerutil.RemoveFinalizer(apiKey, finalizerName)
		if err := r.Update(ctx, apiKey); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *APIKeyReconciler) ensureFinalizer(ctx context.Context, apiKey *v1alpha1.APIKey) error {
	if controllerutil.ContainsFinalizer(apiKey, finalizerName) {
		return nil
	}
	controllerutil.AddFinalizer(apiKey, finalizerName)
	return r.Update(ctx, apiKey)
}

func primaryExpiryFromStatus(apiKey *v1alpha1.APIKey) (*time.Time, bool, error) {
	if apiKey.Status.Primary == nil || apiKey.Status.Primary.KeyID == "" {
		return nil, false, nil
	}
	if apiKey.Status.Primary.ExpiresAt == nil {
		return nil, true, nil
	}
	if apiKey.Status.Primary.ExpiresAt.IsZero() {
		return nil, true, fmt.Errorf("status primary expiry is zero")
	}
	expiresAt := normalizeLifecycleTime(apiKey.Status.Primary.ExpiresAt.Time)
	return &expiresAt, true, nil
}

func (r *APIKeyReconciler) expiryForRekey(apiKey *v1alpha1.APIKey, now time.Time) (*time.Time, error) {
	if apiKey.Status.Primary == nil {
		return initialPrimaryExpiry(apiKey, now)
	}
	return replacementPrimaryExpiry(apiKey, now)
}

func initialPrimaryExpiry(apiKey *v1alpha1.APIKey, now time.Time) (*time.Time, error) {
	if apiKey.Spec.ExpiresIn == "" {
		return nil, nil
	}
	duration, err := time.ParseDuration(apiKey.Spec.ExpiresIn)
	if err != nil {
		return nil, fmt.Errorf("parse expiresIn for new key: %w", err)
	}
	base := apiKey.CreationTimestamp.Time
	if base.IsZero() {
		base = now
	}
	expiresAt := normalizeLifecycleTime(base.Add(duration))
	return &expiresAt, nil
}

func replacementPrimaryExpiry(apiKey *v1alpha1.APIKey, now time.Time) (*time.Time, error) {
	if apiKey.Spec.ExpiresIn == "" {
		return nil, nil
	}
	duration, err := time.ParseDuration(apiKey.Spec.ExpiresIn)
	if err != nil {
		return nil, fmt.Errorf("parse expiresIn for replacement key: %w", err)
	}
	expiresAt := normalizeLifecycleTime(now.Add(duration))
	return &expiresAt, nil
}

func primaryMatchesSecret(apiKey *v1alpha1.APIKey, secret *corev1.Secret) bool {
	if apiKey.Status.Primary == nil || apiKey.Status.Primary.KeyID == "" {
		return false
	}
	rawKey := secret.Data[keyutil.SecretKey]
	return len(rawKey) > 0 && keyutil.KeyIDFromHash(keyutil.HashKey(string(rawKey))) == apiKey.Status.Primary.KeyID
}

func hasExactAPIKeyControllerOwner(obj metav1.Object, apiKey *v1alpha1.APIKey) bool {
	owner := metav1.GetControllerOf(obj)
	return owner != nil &&
		owner.APIVersion == v1alpha1.GroupVersion.String() &&
		owner.Kind == "APIKey" &&
		owner.Name == apiKey.Name &&
		owner.UID != "" &&
		apiKey.UID != "" &&
		owner.UID == apiKey.UID
}

func capDeadline(deadline time.Time, limits ...*time.Time) time.Time {
	for _, limit := range limits {
		if limit != nil && limit.Before(deadline) {
			deadline = *limit
		}
	}
	return normalizeLifecycleTime(deadline)
}

func (r *APIKeyReconciler) replaceKeySecret(
	ctx context.Context,
	apiKey *v1alpha1.APIKey,
	prefix, secretName string,
	primaryExpiresAt *time.Time,
	now time.Time,
	acknowledgeRotation bool,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	primary, err := keyutil.GenerateAPIKey(prefix)
	if err != nil {
		r.emitEventWithAction(apiKey, corev1.EventTypeWarning, eventReasonKeyGenerationFailed, eventActionGeneratePrimaryKey, "Failed to generate primary API key")
		return ctrl.Result{}, fmt.Errorf("failed to generate API key: %w", err)
	}
	secondary, err := keyutil.GenerateAPIKey(prefix)
	if err != nil {
		r.emitEventWithAction(apiKey, corev1.EventTypeWarning, eventReasonKeyGenerationFailed, eventActionGenerateSecondaryKey, "Failed to generate secondary API key")
		return ctrl.Result{}, fmt.Errorf("failed to generate secondary API key: %w", err)
	}

	secondaryExpiresAt := capDeadline(now.Add(r.gracePeriod(apiKey)), primaryExpiresAt)
	data := map[string][]byte{
		keyutil.SecretKey:          []byte(primary.RawKey),
		keyutil.SecondarySecretKey: []byte(secondary.RawKey),
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: apiKey.Namespace,
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
		Data: data,
	}
	if err := r.Create(ctx, secret); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			r.emitEventfWithAction(apiKey, corev1.EventTypeWarning, eventReasonSecretWriteFailed, eventActionCreateSecret, "Failed to create Secret %q", secretName)
			return ctrl.Result{}, fmt.Errorf("failed to create secret: %w", err)
		}
		existing := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: apiKey.Namespace, Name: secretName}, existing); err != nil {
			r.emitEventfWithAction(apiKey, corev1.EventTypeWarning, eventReasonSecretWriteFailed, eventActionReadSecret, "Failed to read existing Secret %q", secretName)
			return ctrl.Result{}, err
		}
		if !hasExactAPIKeyControllerOwner(existing, apiKey) {
			alreadyConflicted := readyConditionHasReason(apiKey, metav1.ConditionFalse, eventReasonSecretConflict)
			message := fmt.Sprintf("Secret %q already exists but is not owned by this APIKey", secretName)
			if !alreadyConflicted {
				if err := r.markNotReady(ctx, apiKey, eventReasonSecretConflict, message, now); err != nil {
					return ctrl.Result{}, err
				}
				r.emitEvent(apiKey, corev1.EventTypeWarning, eventReasonSecretConflict, message)
			}
			return ctrl.Result{}, nil
		}
		if err := r.updateSecretWithRetry(ctx, existing, apiKey, func(s *corev1.Secret) (bool, error) {
			if s.Data == nil {
				s.Data = make(map[string][]byte)
			}
			s.Data[keyutil.SecretKey] = data[keyutil.SecretKey]
			s.Data[keyutil.SecondarySecretKey] = data[keyutil.SecondarySecretKey]
			return true, nil
		}); err != nil {
			r.emitEventfWithAction(apiKey, corev1.EventTypeWarning, eventReasonSecretWriteFailed, eventActionUpdateSecret, "Failed to update existing Secret %q", secretName)
			return ctrl.Result{}, err
		}
	}

	metaNow := metav1.NewTime(now)
	if err := r.updateStatusFromSnapshot(ctx, apiKey, func(obj *v1alpha1.APIKey) {
		obj.Status.SecretRef = &v1alpha1.SecretReference{Name: secretName}
		obj.Status.Primary = &v1alpha1.KeyStatus{KeyID: primary.KeyID}
		if primaryExpiresAt != nil {
			obj.Status.Primary.ExpiresAt = &metav1.Time{Time: normalizeLifecycleTime(*primaryExpiresAt)}
		}
		obj.Status.Secondary = &v1alpha1.KeyStatus{
			KeyID:     secondary.KeyID,
			ExpiresAt: &metav1.Time{Time: secondaryExpiresAt},
		}
		obj.Status.Phase = v1alpha1.PhaseActive
		obj.Status.LastSyncedAt = &metaNow
		reason := "Synced"
		message := "Key is active"
		if acknowledgeRotation {
			obj.Status.LastRotatedAt = &metaNow
			reason = eventReasonRotated
			message = "Key rotation recovered with fresh key material"
		}
		meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: metaNow,
		})
	}); err != nil {
		return ctrl.Result{}, err
	}

	r.emitEventf(apiKey, corev1.EventTypeNormal, eventReasonCreated, "Stored newly generated API key material in Secret %q", secretName)
	logger.Info("Stored fresh Secret for APIKey", "namespace", apiKey.Namespace, "primaryKeyID", primary.KeyID, "secondaryKeyID", secondary.KeyID)
	return ctrl.Result{RequeueAfter: time.Second}, nil
}

func (r *APIKeyReconciler) gracePeriod(apiKey *v1alpha1.APIKey) time.Duration {
	if apiKey.Spec.Rotation != nil && apiKey.Spec.Rotation.GracePeriod != "" {
		if d, err := time.ParseDuration(apiKey.Spec.Rotation.GracePeriod); err == nil {
			return d
		}
	}
	return v1alpha1.DefaultGracePeriod
}

func (r *APIKeyReconciler) isRotationDue(apiKey *v1alpha1.APIKey, now time.Time) bool {
	if apiKey.Spec.Rotation == nil || apiKey.Spec.Rotation.RotateAt == nil {
		return false
	}
	rotateAt := normalizeLifecycleTime(apiKey.Spec.Rotation.RotateAt.Time)
	if rotateAt.After(now) {
		return false
	}
	return apiKey.Status.LastRotatedAt == nil || apiKey.Status.LastRotatedAt.Time.Before(rotateAt)
}

func (r *APIKeyReconciler) reconcileSecondary(ctx context.Context, apiKey *v1alpha1.APIKey, secret *corev1.Secret, now time.Time) error {
	status := apiKey.Status.Secondary
	rawKey := secret.Data[keyutil.SecondarySecretKey]
	remove := false
	if status == nil {
		remove = len(rawKey) > 0
	} else if status.KeyID == "" || status.ExpiresAt == nil || status.ExpiresAt.IsZero() {
		remove = true
	} else if !now.Before(status.ExpiresAt.Time) {
		remove = true
	} else if len(rawKey) == 0 || keyutil.KeyIDFromHash(keyutil.HashKey(string(rawKey))) != status.KeyID {
		remove = true
	}
	if !remove {
		return nil
	}

	if len(rawKey) > 0 {
		if err := r.updateSecretWithRetry(ctx, secret, apiKey, func(s *corev1.Secret) (bool, error) {
			if len(s.Data[keyutil.SecondarySecretKey]) == 0 {
				return false, nil
			}
			delete(s.Data, keyutil.SecondarySecretKey)
			return true, nil
		}); err != nil {
			return err
		}
	}
	if status != nil {
		if err := r.updateStatusWithRetry(ctx, apiKey, func(obj *v1alpha1.APIKey) {
			obj.Status.Secondary = nil
		}); err != nil {
			return err
		}
	}
	r.emitEventf(apiKey, corev1.EventTypeNormal, eventReasonSecondaryKeyRemoved, "Removed incoherent or expired secondary API key from Secret %q", secret.Name)
	return nil
}

func (r *APIKeyReconciler) ensureActiveStatus(ctx context.Context, apiKey *v1alpha1.APIKey, now time.Time) error {
	condition := meta.FindStatusCondition(apiKey.Status.Conditions, "Ready")
	if apiKey.Status.Phase == v1alpha1.PhaseActive && condition != nil && condition.Status == metav1.ConditionTrue && condition.Reason == "Synced" {
		return nil
	}
	return r.updateStatusWithRetry(ctx, apiKey, func(obj *v1alpha1.APIKey) {
		metaNow := metav1.NewTime(now)
		obj.Status.Phase = v1alpha1.PhaseActive
		obj.Status.LastSyncedAt = &metaNow
		meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Synced",
			Message:            "Key is active",
			LastTransitionTime: metaNow,
		})
	})
}

func (r *APIKeyReconciler) computeRequeueAfter(apiKey *v1alpha1.APIKey, primaryExpiresAt *time.Time, now time.Time) time.Duration {
	var earliest time.Duration
	if primaryExpiresAt != nil {
		if d := primaryExpiresAt.Sub(now); d > 0 {
			earliest = d
		}
	}
	if apiKey.Status.Secondary != nil && apiKey.Status.Secondary.ExpiresAt != nil {
		if d := apiKey.Status.Secondary.ExpiresAt.Sub(now); d > 0 && (earliest == 0 || d < earliest) {
			earliest = d
		}
	}
	if apiKey.Spec.Rotation != nil && apiKey.Spec.Rotation.RotateAt != nil {
		if d := apiKey.Spec.Rotation.RotateAt.Sub(now); d > 0 && (earliest == 0 || d < earliest) {
			earliest = d
		}
	}
	return earliest
}

func (r *APIKeyReconciler) handleRotation(ctx context.Context, apiKey *v1alpha1.APIKey, secret *corev1.Secret, prefix string, now time.Time) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	oldPrimaryExpiresAt, _, err := primaryExpiryFromStatus(apiKey)
	if err != nil {
		return ctrl.Result{}, err
	}
	newExpiresAt, err := replacementPrimaryExpiry(apiKey, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	result, err := keyutil.GenerateAPIKey(prefix)
	if err != nil {
		r.emitEventWithAction(apiKey, corev1.EventTypeWarning, eventReasonRotationFailed, eventActionGenerateReplacementKey, "Failed to generate replacement API key")
		return ctrl.Result{}, fmt.Errorf("failed to generate API key for rotation: %w", err)
	}

	oldPrimaryKeyID := apiKey.Status.Primary.KeyID
	secondaryExpiresAt := capDeadline(now.Add(r.gracePeriod(apiKey)), oldPrimaryExpiresAt, newExpiresAt)
	if err := r.updateSecretWithRetry(ctx, secret, apiKey, func(s *corev1.Secret) (bool, error) {
		primaryKey := s.Data[keyutil.SecretKey]
		if len(primaryKey) == 0 {
			return false, fmt.Errorf("secret %s/%s primary API key missing or empty, cannot rotate", s.Namespace, s.Name)
		}
		if keyutil.KeyIDFromHash(keyutil.HashKey(string(primaryKey))) != oldPrimaryKeyID {
			return false, fmt.Errorf("secret %s/%s primary API key changed before rotation", s.Namespace, s.Name)
		}
		if s.Data == nil {
			s.Data = make(map[string][]byte)
		}
		s.Data[keyutil.SecondarySecretKey] = append([]byte(nil), primaryKey...)
		s.Data[keyutil.SecretKey] = []byte(result.RawKey)
		return true, nil
	}); err != nil {
		r.emitEventfWithAction(apiKey, corev1.EventTypeWarning, eventReasonRotationFailed, eventActionRotateSecret, "Failed to update Secret %q with rotated API key material", secret.Name)
		return ctrl.Result{}, fmt.Errorf("failed to update secret for rotation: %w", err)
	}

	metaNow := metav1.NewTime(now)
	if err := r.updateStatusFromSnapshot(ctx, apiKey, func(obj *v1alpha1.APIKey) {
		obj.Status.Primary = &v1alpha1.KeyStatus{KeyID: result.KeyID}
		if newExpiresAt != nil {
			obj.Status.Primary.ExpiresAt = &metav1.Time{Time: *newExpiresAt}
		}
		obj.Status.Secondary = &v1alpha1.KeyStatus{
			KeyID:     oldPrimaryKeyID,
			ExpiresAt: &metav1.Time{Time: secondaryExpiresAt},
		}
		obj.Status.Phase = v1alpha1.PhaseActive
		obj.Status.LastSyncedAt = &metaNow
		obj.Status.LastRotatedAt = &metaNow
		meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             eventReasonRotated,
			Message:            fmt.Sprintf("Key rotated, secondary key valid until %s", secondaryExpiresAt.Format(time.RFC3339)),
			LastTransitionTime: metaNow,
		})
	}); err != nil {
		r.emitEventfWithAction(apiKey, corev1.EventTypeWarning, eventReasonRotationFailed, eventActionPersistRotationStatus, "Failed to persist APIKey status after rotating Secret %q", secret.Name)
		return ctrl.Result{}, err
	}

	r.emitEventf(apiKey, corev1.EventTypeNormal, eventReasonRotated, "Key rotated, secondary key valid until %s", secondaryExpiresAt.Format(time.RFC3339))
	logger.Info("APIKey rotated", "namespace", apiKey.Namespace, "newKeyID", result.KeyID, "secondaryKeyID", oldPrimaryKeyID, "gracePeriodEnds", secondaryExpiresAt.Format(time.RFC3339))
	if requeueAfter := secondaryExpiresAt.Sub(now); requeueAfter > 0 {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	return ctrl.Result{RequeueAfter: time.Second}, nil
}

func (r *APIKeyReconciler) handleExpiry(ctx context.Context, apiKey *v1alpha1.APIKey, expiresAt *time.Time, now time.Time) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	expiresAtValue := normalizeLifecycleTime(*expiresAt)
	alreadyExpired := readyConditionHasReason(apiKey, metav1.ConditionFalse, eventReasonExpired)
	expiryMessage := fmt.Sprintf("Key expired at %s", expiresAtValue.Format(time.RFC3339))

	cleanupComplete, err := r.deleteDeclaredKeySecret(ctx, apiKey)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !cleanupComplete {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if err := r.updateStatusWithRetry(ctx, apiKey, func(obj *v1alpha1.APIKey) {
		obj.Status.Phase = v1alpha1.PhaseExpired
		obj.Status.Secondary = nil
		obj.Status.SecretRef = nil
		meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             eventReasonExpired,
			Message:            expiryMessage,
			LastTransitionTime: metav1.NewTime(now),
		})
	}); err != nil {
		return ctrl.Result{}, err
	}
	if !alreadyExpired {
		r.emitEvent(apiKey, corev1.EventTypeWarning, eventReasonExpired, expiryMessage)
	}
	logger.Info("APIKey expired", "namespace", apiKey.Namespace, "keyID", primaryKeyID(apiKey), "expiresAt", expiresAtValue.Format(time.RFC3339))
	return ctrl.Result{}, nil
}

// deleteDeclaredKeySecret reports completion only after the declared owned
// Secret is no longer readable. An accepted deletion can leave a terminating
// Secret behind while finalizers run, so callers retain status.secretRef and
// retry rather than forgetting plaintext that still exists.
func (r *APIKeyReconciler) deleteDeclaredKeySecret(ctx context.Context, apiKey *v1alpha1.APIKey) (bool, error) {
	if apiKey.Status.SecretRef == nil || apiKey.Status.SecretRef.Name == "" {
		return true, nil
	}
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		// Cleanup completion must not rely on a cached NotFound: a same-name
		// replacement may exist before its informer event has arrived.
		reader = r.APIReader
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: apiKey.Namespace, Name: apiKey.Status.SecretRef.Name}
	if err := reader.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if !hasExactAPIKeyControllerOwner(secret, apiKey) {
		return true, nil
	}
	if !secret.DeletionTimestamp.IsZero() {
		return false, nil
	}
	if secret.UID == "" {
		return false, fmt.Errorf("cannot delete owned Secret %q without a UID precondition", secret.Name)
	}
	uid := secret.UID
	if err := r.Delete(ctx, secret, client.Preconditions{UID: &uid}); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}

	remaining := &corev1.Secret{}
	if err := reader.Get(ctx, key, remaining); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

func (r *APIKeyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.APIKey{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.secretToAPIKey)).
		Complete(r)
}

func primaryKeyID(apiKey *v1alpha1.APIKey) string {
	if apiKey.Status.Primary != nil && apiKey.Status.Primary.KeyID != "" {
		return apiKey.Status.Primary.KeyID
	}
	return "<none>"
}

func (r *APIKeyReconciler) secretToAPIKey(_ context.Context, obj client.Object) []reconcile.Request {
	owner := metav1.GetControllerOf(obj)
	ownedByAPIKey := owner != nil &&
		owner.APIVersion == v1alpha1.GroupVersion.String() &&
		owner.Kind == "APIKey"
	if obj.GetName() != keyutil.SecretName && !ownedByAPIKey {
		return nil
	}
	// APIKey is a namespace singleton. Mapping the fixed Secret name even when
	// it is unowned ensures conflict deletion wakes the Pending APIKey.
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: obj.GetNamespace(),
		Name:      keyutil.SingletonName,
	}}}
}
