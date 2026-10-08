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
	"errors"
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

var errSecretKeyBindingMismatch = errors.New("secret key material does not match its expiry metadata")

// APIKeyReconciler reconciles APIKey objects.
type APIKeyReconciler struct {
	client.Client
	APIReader client.Reader
	Recorder  events.EventRecorder
	Clock     clock.PassiveClock
}

func (r *APIKeyReconciler) authoritativeReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
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

func normalizeLifecycleDeadline(value time.Time) time.Time {
	return normalizeLifecycleTime(value)
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
	prefix := apiKey.Spec.Prefix
	if prefix == "" {
		prefix = keyutil.DefaultPrefix
	}

	// Without a referenced Secret, status describes an existing key that needs
	// regeneration; spec.expiresIn is consulted only for a genuinely new key.
	if apiKey.Status.SecretRef == nil {
		expiresAt, err := r.primaryExpiryWithoutSecret(&apiKey)
		if err != nil {
			return ctrl.Result{}, err
		}
		if expiresAt != nil && !now.Before(*expiresAt) {
			return ctrl.Result{}, r.handleExpiry(ctx, &apiKey, expiresAt, now)
		}
		return r.createKeySecret(ctx, &apiKey, prefix, expiresAt, now)
	}

	// Existing Secret metadata is authoritative for the current key. Resolve and
	// repair it before checking expiry so a pending expiresIn change or stale
	// post-rotation status cannot shorten or extend the key already in the Secret.
	secret, requeueNeeded, err := r.fetchKeySecret(ctx, &apiKey)
	if err != nil {
		return ctrl.Result{}, err
	}
	if requeueNeeded {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if rawKey := secret.Data[keyutil.SecretKey]; len(rawKey) == 0 {
		statusExpiry, found, statusErr := primaryExpiryFromStatus(&apiKey)
		if statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		if found && statusExpiry != nil && !now.Before(*statusExpiry) {
			return ctrl.Result{}, r.handleExpiry(ctx, &apiKey, statusExpiry, now)
		}
		return r.handleCorruptSecret(ctx, &apiKey, secret.Name)
	}

	if secretNeedsRekey(&apiKey, secret) {
		expiresAt, expiryErr := r.rekeyPrimaryExpiry(&apiKey, now)
		if expiryErr != nil {
			return ctrl.Result{}, expiryErr
		}
		if expiresAt != nil && !now.Before(*expiresAt) {
			return ctrl.Result{}, r.handleExpiry(ctx, &apiKey, expiresAt, now)
		}
		logger.Info("Replacing key material whose expiry metadata cannot be bound authoritatively", "namespace", apiKey.Namespace)
		return r.createKeySecret(ctx, &apiKey, prefix, expiresAt, now)
	}

	metadata, err := r.reconcileSecretExpiryMetadata(ctx, &apiKey, secret)
	if err != nil {
		if errors.Is(err, errSecretKeyBindingMismatch) {
			expiresAt, expiryErr := replacementPrimaryExpiry(&apiKey, now)
			if expiryErr != nil {
				return ctrl.Result{}, expiryErr
			}
			return r.createKeySecret(ctx, &apiKey, prefix, expiresAt, now)
		}
		return ctrl.Result{}, err
	}

	rotationDue := r.isRotationDue(&apiKey, secret, now)
	if rotationAlreadyApplied(&apiKey, secret) && !rotationDue {
		return r.recoverRotationStatus(ctx, &apiKey, secret, metadata, now)
	}

	primaryExpired := metadata.Primary != nil && !now.Before(*metadata.Primary)
	if primaryExpired && (!rotationDue || apiKey.Spec.Rotation.RotateAt.After(*metadata.Primary)) {
		return ctrl.Result{}, r.handleExpiry(ctx, &apiKey, metadata.Primary, now)
	}
	if rotationDue {
		return r.handleRotation(ctx, &apiKey, secret, prefix, now)
	}
	if primaryExpired {
		return ctrl.Result{}, r.handleExpiry(ctx, &apiKey, metadata.Primary, now)
	}

	if err := r.cleanupExpiredSecondaryKey(ctx, &apiKey, secret, metadata.Secondary, now); err != nil {
		return ctrl.Result{}, err
	}
	if metadata.Secondary != nil && !now.Before(*metadata.Secondary) {
		metadata.Secondary = nil
	}

	rawKey := string(secret.Data[keyutil.SecretKey])
	keyID := keyutil.KeyIDFromHash(keyutil.HashKey(rawKey))
	secondaryStatus := keyStatusForSecretKey(secret.Data[keyutil.SecondarySecretKey], metadata.Secondary)
	if err := r.updateActiveStatus(ctx, &apiKey, keyID, metadata.Primary, secondaryStatus, now); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("APIKey synced", "namespace", apiKey.Namespace, "keyID", keyID)
	return ctrl.Result{RequeueAfter: r.computeRequeueAfter(&apiKey, metadata.Primary, metadata.Secondary, now)}, nil
}

// updateStatusWithRetry re-fetches the APIKey on conflict and applies the
// mutate function before each status update attempt.
func (r *APIKeyReconciler) updateStatusWithRetry(ctx context.Context, apiKey *v1alpha1.APIKey, mutate func(*v1alpha1.APIKey)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, types.NamespacedName{Namespace: apiKey.Namespace, Name: apiKey.Name}, apiKey); err != nil {
			return err
		}
		mutate(apiKey)
		return r.Status().Update(ctx, apiKey)
	})
}

// updateSecretWithRetry re-fetches the Secret on conflict, verifies ownership,
// and applies the mutate function before each update attempt. Mutations report
// whether an update is needed so steady-state reconciliation remains a no-op.
func (r *APIKeyReconciler) updateSecretWithRetry(ctx context.Context, secret *corev1.Secret, apiKey *v1alpha1.APIKey, mutate func(*corev1.Secret) (bool, error)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, types.NamespacedName{Namespace: secret.Namespace, Name: secret.Name}, secret); err != nil {
			return err
		}
		if !metav1.IsControlledBy(secret, apiKey) {
			return fmt.Errorf("secret %s/%s is not owned by APIKey %s", secret.Namespace, secret.Name, apiKey.Name)
		}
		changed, err := mutate(secret)
		if err != nil || !changed {
			return err
		}
		return r.Update(ctx, secret)
	})
}

// markNotReady sets the Ready condition to False and Phase to Pending.
func (r *APIKeyReconciler) markNotReady(ctx context.Context, apiKey *v1alpha1.APIKey, reason, message string) error {
	return r.updateStatusWithRetry(ctx, apiKey, func(obj *v1alpha1.APIKey) {
		meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: metav1.Now(),
		})
		obj.Status.Phase = v1alpha1.PhasePending
	})
}

// handleDeletion cleans up the finalizer when an APIKey is deleted.
func (r *APIKeyReconciler) handleDeletion(ctx context.Context, apiKey *v1alpha1.APIKey) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	logger.Info("APIKey being deleted", "namespace", apiKey.Namespace, "keyID", primaryKeyID(apiKey))

	if controllerutil.ContainsFinalizer(apiKey, finalizerName) {
		controllerutil.RemoveFinalizer(apiKey, finalizerName)
		if err := r.Update(ctx, apiKey); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// ensureFinalizer adds the cleanup finalizer if not already present.
func (r *APIKeyReconciler) ensureFinalizer(ctx context.Context, apiKey *v1alpha1.APIKey) error {
	if !controllerutil.ContainsFinalizer(apiKey, finalizerName) {
		controllerutil.AddFinalizer(apiKey, finalizerName)
		return r.Update(ctx, apiKey)
	}
	return nil
}

// primaryExpiryWithoutSecret returns the persisted current-key lifetime when
// status identifies an existing key. spec.expiresIn is used only before the
// first key has been created.
func (r *APIKeyReconciler) primaryExpiryWithoutSecret(apiKey *v1alpha1.APIKey) (*time.Time, error) {
	if expiresAt, found, err := primaryExpiryFromStatus(apiKey); found || err != nil {
		return expiresAt, err
	}
	return initialPrimaryExpiry(apiKey)
}

func primaryExpiryFromStatus(apiKey *v1alpha1.APIKey) (*time.Time, bool, error) {
	if apiKey.Status.Primary == nil {
		return nil, false, nil
	}
	if apiKey.Status.Primary.ExpiresAt == nil {
		return nil, true, nil
	}
	if apiKey.Status.Primary.ExpiresAt.IsZero() {
		return nil, true, fmt.Errorf("status primary expiry is zero")
	}
	expiresAt := normalizeLifecycleDeadline(apiKey.Status.Primary.ExpiresAt.Time)
	return &expiresAt, true, nil
}

func initialPrimaryExpiry(apiKey *v1alpha1.APIKey) (*time.Time, error) {
	if apiKey.Spec.ExpiresIn == "" {
		return nil, nil
	}
	duration, err := time.ParseDuration(apiKey.Spec.ExpiresIn)
	if err != nil {
		return nil, fmt.Errorf("parse expiresIn for new key: %w", err)
	}
	expiresAt := normalizeLifecycleDeadline(apiKey.CreationTimestamp.Add(duration))
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
	expiresAt := normalizeLifecycleDeadline(now.Add(duration))
	return &expiresAt, nil
}

func (r *APIKeyReconciler) rekeyPrimaryExpiry(apiKey *v1alpha1.APIKey, now time.Time) (*time.Time, error) {
	if apiKey.Status.Primary == nil {
		return initialPrimaryExpiry(apiKey)
	}
	return replacementPrimaryExpiry(apiKey, now)
}

func secretNeedsRekey(apiKey *v1alpha1.APIKey, secret *corev1.Secret) bool {
	if !metav1.IsControlledBy(secret, apiKey) {
		return false
	}
	primaryRaw := secret.Data[keyutil.SecretKey]
	primaryHash := keyutil.HashKey(string(primaryRaw))
	if boundHash, ok := secret.Annotations[keyutil.PrimaryHashAnnotation]; ok && boundHash != primaryHash {
		return true
	}

	secondaryRaw := secret.Data[keyutil.SecondarySecretKey]
	if len(secondaryRaw) > 0 {
		secondaryHash := keyutil.HashKey(string(secondaryRaw))
		if boundHash, ok := secret.Annotations[keyutil.SecondaryHashAnnotation]; ok && boundHash != secondaryHash {
			return true
		}
	}

	if _, err := keyutil.ParsePrimaryExpiry(secret.Annotations); err == nil {
		return false
	}
	if apiKey.Status.Primary == nil || apiKey.Status.Primary.KeyID == "" {
		return true
	}
	return apiKey.Status.Primary.KeyID != keyutil.KeyIDFromHash(primaryHash)
}

// handleExpiry deletes the owned key material before marking the APIKey expired.
// Status is deliberately written second so retries cannot leave an expired key
// available in a Secret after the controller reports the transition complete.
func (r *APIKeyReconciler) handleExpiry(ctx context.Context, apiKey *v1alpha1.APIKey, expiresAt *time.Time, now time.Time) error {
	logger := log.FromContext(ctx)
	normalizedExpiry := normalizeLifecycleDeadline(*expiresAt)
	alreadyExpired := readyConditionHasReason(apiKey, metav1.ConditionFalse, eventReasonExpired)
	expiryMessage := fmt.Sprintf("Key expired at %s", normalizedExpiry.Format(time.RFC3339))

	if err := r.deleteOwnedKeySecret(ctx, apiKey); err != nil {
		return err
	}

	if err := r.updateStatusWithRetry(ctx, apiKey, func(obj *v1alpha1.APIKey) {
		obj.Status.Phase = v1alpha1.PhaseExpired
		if obj.Status.Primary == nil {
			obj.Status.Primary = &v1alpha1.KeyStatus{}
		}
		obj.Status.Primary.ExpiresAt = &metav1.Time{Time: normalizedExpiry}
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
		return err
	}
	if !alreadyExpired {
		r.emitEvent(apiKey, corev1.EventTypeWarning, eventReasonExpired, expiryMessage)
	}
	logger.Info("APIKey expired", "namespace", apiKey.Namespace, "keyID", primaryKeyID(apiKey), "expiresAt", normalizedExpiry.Format(time.RFC3339))
	return nil
}

func (r *APIKeyReconciler) deleteOwnedKeySecret(ctx context.Context, apiKey *v1alpha1.APIKey) error {
	names := make([]string, 0, 2)
	if apiKey.Status.SecretRef != nil && apiKey.Status.SecretRef.Name != "" {
		names = append(names, apiKey.Status.SecretRef.Name)
	}
	if len(names) == 0 || names[0] != keyutil.SecretName {
		names = append(names, keyutil.SecretName)
	}

	reader := r.authoritativeReader()
	for _, name := range names {
		key := types.NamespacedName{Namespace: apiKey.Namespace, Name: name}
		for {
			secret := &corev1.Secret{}
			err := reader.Get(ctx, key, secret)
			if apierrors.IsNotFound(err) {
				break
			}
			if err != nil {
				return err
			}
			if !metav1.IsControlledBy(secret, apiKey) {
				break
			}

			deleteOptions := make([]client.DeleteOption, 0, 1)
			if secret.UID != "" {
				uid := secret.UID
				deleteOptions = append(deleteOptions, client.Preconditions{UID: &uid})
			}
			if err := r.Delete(ctx, secret, deleteOptions...); err != nil && !apierrors.IsNotFound(err) {
				return err
			}

			remaining := &corev1.Secret{}
			err = reader.Get(ctx, key, remaining)
			if apierrors.IsNotFound(err) {
				break
			}
			if err != nil {
				return err
			}
			if remaining.UID == secret.UID {
				return fmt.Errorf("wait for owned Secret %q deletion: Secret still exists", name)
			}
			if !metav1.IsControlledBy(remaining, apiKey) {
				break
			}
			// A same-name Secret controlled by this APIKey appeared after the
			// UID-preconditioned delete. Delete that replacement before status
			// can claim all owned key material is gone.
		}
	}
	return nil
}

// createKeySecret generates two API keys (primary + secondary) at creation time,
// creates the Secret, and requeues. The secondary key is valid for the grace
// period so clients can pre-stage rotation from day one.
func (r *APIKeyReconciler) createKeySecret(ctx context.Context, apiKey *v1alpha1.APIKey, prefix string, primaryExpiresAt *time.Time, now time.Time) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	primaryExpiresAt = normalizeOptionalDeadline(primaryExpiresAt)
	primaryExpiryValue, err := keyutil.FormatPrimaryExpiry(primaryExpiresAt)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("format primary expiry: %w", err)
	}
	secondaryExpiresAt := normalizeLifecycleDeadline(capExpiry(now.Add(r.gracePeriod(apiKey)), primaryExpiresAt))
	secondaryExpiryValue, err := keyutil.FormatExpiry(secondaryExpiresAt)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("format secondary expiry: %w", err)
	}

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

	data := map[string][]byte{
		keyutil.SecretKey:          []byte(primary.RawKey),
		keyutil.SecondarySecretKey: []byte(secondary.RawKey),
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keyutil.SecretName,
			Namespace: apiKey.Namespace,
			Annotations: map[string]string{
				keyutil.PrimaryExpiryAnnotation:   primaryExpiryValue,
				keyutil.SecondaryExpiryAnnotation: secondaryExpiryValue,
				keyutil.PrimaryHashAnnotation:     primary.Hash,
				keyutil.SecondaryHashAnnotation:   secondary.Hash,
			},
			Labels: map[string]string{
				"kaito.sh/managed-by": "llm-auth-service",
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
			r.emitEventfWithAction(apiKey, corev1.EventTypeWarning, eventReasonSecretWriteFailed, eventActionCreateSecret, "Failed to create Secret %q", keyutil.SecretName)
			return ctrl.Result{}, fmt.Errorf("failed to create secret: %w", err)
		}
		// Secret already exists – verify it is owned by this APIKey before updating
		existing := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: apiKey.Namespace, Name: keyutil.SecretName}, existing); err != nil {
			r.emitEventfWithAction(apiKey, corev1.EventTypeWarning, eventReasonSecretWriteFailed, eventActionReadSecret, "Failed to read existing Secret %q", keyutil.SecretName)
			return ctrl.Result{}, err
		}
		if !metav1.IsControlledBy(existing, apiKey) {
			alreadyConflicted := readyConditionHasReason(apiKey, metav1.ConditionFalse, eventReasonSecretConflict)
			message := fmt.Sprintf("Secret %q already exists but is not owned by this APIKey", keyutil.SecretName)
			if err := r.markNotReady(ctx, apiKey, eventReasonSecretConflict, message); err != nil {
				return ctrl.Result{}, err
			}
			if !alreadyConflicted {
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
			if s.Annotations == nil {
				s.Annotations = make(map[string]string)
			}
			s.Annotations[keyutil.PrimaryExpiryAnnotation] = primaryExpiryValue
			s.Annotations[keyutil.SecondaryExpiryAnnotation] = secondaryExpiryValue
			s.Annotations[keyutil.PrimaryHashAnnotation] = primary.Hash
			s.Annotations[keyutil.SecondaryHashAnnotation] = secondary.Hash
			delete(s.Annotations, keyutil.RotationRequestAnnotation)
			delete(s.Annotations, keyutil.LastRotatedAtAnnotation)
			return true, nil
		}); err != nil {
			r.emitEventfWithAction(apiKey, corev1.EventTypeWarning, eventReasonSecretWriteFailed, eventActionUpdateSecret, "Failed to update existing Secret %q", keyutil.SecretName)
			return ctrl.Result{}, err
		}
	}

	if err := r.updateStatusWithRetry(ctx, apiKey, func(obj *v1alpha1.APIKey) {
		metaNow := metav1.NewTime(now)
		obj.Status.SecretRef = &v1alpha1.SecretReference{Name: keyutil.SecretName}
		obj.Status.Primary = &v1alpha1.KeyStatus{KeyID: primary.KeyID}
		if primaryExpiresAt != nil {
			obj.Status.Primary.ExpiresAt = &metav1.Time{Time: *primaryExpiresAt}
		}
		obj.Status.Secondary = &v1alpha1.KeyStatus{
			KeyID:     secondary.KeyID,
			ExpiresAt: &metav1.Time{Time: secondaryExpiresAt},
		}
		obj.Status.Phase = v1alpha1.PhaseActive
		obj.Status.LastSyncedAt = &metaNow
		meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Synced",
			Message:            "Key is active",
			LastTransitionTime: metaNow,
		})
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.emitEventf(apiKey, corev1.EventTypeNormal, eventReasonCreated, "Stored newly generated API key material in Secret %q", keyutil.SecretName)
	logger.Info("Secret created for APIKey", "namespace", apiKey.Namespace, "primaryKeyID", primary.KeyID, "secondaryKeyID", secondary.KeyID)
	return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
}

// gracePeriod returns the configured grace period for the APIKey, defaulting
// to v1alpha1.DefaultGracePeriod when unset or unparseable.
func (r *APIKeyReconciler) gracePeriod(apiKey *v1alpha1.APIKey) time.Duration {
	if apiKey.Spec.Rotation != nil && apiKey.Spec.Rotation.GracePeriod != "" {
		if d, err := time.ParseDuration(apiKey.Spec.Rotation.GracePeriod); err == nil {
			return d
		}
	}
	return v1alpha1.DefaultGracePeriod
}

func normalizeOptionalDeadline(deadline *time.Time) *time.Time {
	if deadline == nil {
		return nil
	}
	normalized := normalizeLifecycleDeadline(*deadline)
	return &normalized
}

func capExpiry(deadline time.Time, primaryExpiresAt *time.Time) time.Time {
	if primaryExpiresAt != nil && primaryExpiresAt.Before(deadline) {
		return *primaryExpiresAt
	}
	return deadline
}

// fetchKeySecret retrieves the Secret referenced by the APIKey status. If the
// Secret is missing, it clears the reference and signals that a requeue is
// needed. Primary key corruption is handled only after authoritative expiry
// metadata has been reconciled and evaluated.
func (r *APIKeyReconciler) fetchKeySecret(ctx context.Context, apiKey *v1alpha1.APIKey) (*corev1.Secret, bool, error) {
	logger := log.FromContext(ctx)
	var secret corev1.Secret
	secretNN := types.NamespacedName{
		Namespace: apiKey.Namespace,
		Name:      apiKey.Status.SecretRef.Name,
	}
	if err := r.Get(ctx, secretNN, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Secret not found, will regenerate", "namespace", apiKey.Namespace, "keyID", primaryKeyID(apiKey), "secret", secretNN.Name)
			if err := r.clearSecretRef(ctx, apiKey); err != nil {
				return nil, true, err
			}
			r.emitEventf(apiKey, corev1.EventTypeWarning, eventReasonSecretMissing, "Secret %q was not found; API key material will be regenerated", secretNN.Name)
			return nil, true, nil
		}
		return nil, false, err
	}

	return &secret, false, nil
}

func (r *APIKeyReconciler) handleCorruptSecret(ctx context.Context, apiKey *v1alpha1.APIKey, secretName string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Secret has empty key field, will regenerate", "namespace", apiKey.Namespace, "keyID", primaryKeyID(apiKey), "secret", secretName)
	if err := r.clearSecretRef(ctx, apiKey); err != nil {
		return ctrl.Result{}, err
	}
	r.emitEventf(apiKey, corev1.EventTypeWarning, eventReasonSecretCorrupt, "Secret %q has no primary API key; API key material will be regenerated", secretName)
	return ctrl.Result{RequeueAfter: time.Second}, nil
}

// secretExpiryMetadata captures authoritative key deadlines while migrating
// Secrets created before expiry annotations were introduced. Valid Secret
// annotations are authoritative. Missing or invalid legacy metadata is repaired
// from status without granting a fresh lifetime, and secondary material without
// an authoritative deadline is removed.
type secretExpiryMetadata struct {
	Primary   *time.Time
	Secondary *time.Time
}

func (r *APIKeyReconciler) reconcileSecretExpiryMetadata(ctx context.Context, apiKey *v1alpha1.APIKey, secret *corev1.Secret) (*secretExpiryMetadata, error) {
	if !metav1.IsControlledBy(secret, apiKey) {
		return expiryMetadataForUnownedSecret(apiKey, secret)
	}

	var metadata *secretExpiryMetadata
	if err := r.updateSecretWithRetry(ctx, secret, apiKey, func(s *corev1.Secret) (bool, error) {
		resolved, changed, err := resolveSecretExpiryMetadata(apiKey, s)
		if err != nil {
			return false, err
		}
		metadata = resolved
		return changed, nil
	}); err != nil {
		return nil, err
	}
	return metadata, nil
}

func resolveSecretExpiryMetadata(apiKey *v1alpha1.APIKey, secret *corev1.Secret) (*secretExpiryMetadata, bool, error) {
	metadata := &secretExpiryMetadata{}
	changed := false
	if secret.Annotations == nil {
		secret.Annotations = make(map[string]string)
	}

	primaryRaw := secret.Data[keyutil.SecretKey]
	if len(primaryRaw) == 0 {
		return nil, false, fmt.Errorf("cannot bind expiry metadata to an empty primary key")
	}
	primaryHash := keyutil.HashKey(string(primaryRaw))
	if boundHash, ok := secret.Annotations[keyutil.PrimaryHashAnnotation]; ok {
		if boundHash != primaryHash {
			return nil, false, errSecretKeyBindingMismatch
		}
	} else {
		secret.Annotations[keyutil.PrimaryHashAnnotation] = primaryHash
		changed = true
	}

	primary, err := keyutil.ParsePrimaryExpiry(secret.Annotations)
	if err == nil {
		if !primary.IsZero() {
			metadata.Primary = &primary
		}
	} else {
		fallback, found, statusErr := primaryExpiryFromStatus(apiKey)
		if statusErr != nil {
			return nil, false, statusErr
		}
		if !found {
			return nil, false, fmt.Errorf("cannot repair primary expiry metadata without authoritative status: %w", err)
		}
		metadata.Primary = fallback
		primaryValue, formatErr := keyutil.FormatPrimaryExpiry(fallback)
		if formatErr != nil {
			return nil, false, fmt.Errorf("format primary expiry for backfill: %w", formatErr)
		}
		secret.Annotations[keyutil.PrimaryExpiryAnnotation] = primaryValue
		changed = true
	}

	secondaryRaw := secret.Data[keyutil.SecondarySecretKey]
	if len(secondaryRaw) == 0 {
		if _, ok := secret.Data[keyutil.SecondarySecretKey]; ok {
			delete(secret.Data, keyutil.SecondarySecretKey)
			changed = true
		}
		for _, annotation := range []string{keyutil.SecondaryExpiryAnnotation, keyutil.SecondaryHashAnnotation} {
			if _, ok := secret.Annotations[annotation]; ok {
				delete(secret.Annotations, annotation)
				changed = true
			}
		}
		return metadata, changed, nil
	}

	secondaryHash := keyutil.HashKey(string(secondaryRaw))
	if boundHash, ok := secret.Annotations[keyutil.SecondaryHashAnnotation]; ok {
		if boundHash != secondaryHash {
			return nil, false, errSecretKeyBindingMismatch
		}
	} else {
		secret.Annotations[keyutil.SecondaryHashAnnotation] = secondaryHash
		changed = true
	}

	secondary, secondaryErr := keyutil.ParseSecondaryExpiry(secret.Annotations)
	if secondaryErr != nil {
		fallback, found := secondaryExpiryFromStatus(apiKey)
		if !found {
			delete(secret.Data, keyutil.SecondarySecretKey)
			delete(secret.Annotations, keyutil.SecondaryExpiryAnnotation)
			delete(secret.Annotations, keyutil.SecondaryHashAnnotation)
			return metadata, true, nil
		}
		secondary = *fallback
	}

	cappedSecondary := capExpiry(secondary, metadata.Primary)
	metadata.Secondary = &cappedSecondary
	if secondaryErr != nil || !cappedSecondary.Equal(secondary) {
		secondaryValue, formatErr := keyutil.FormatExpiry(cappedSecondary)
		if formatErr != nil {
			return nil, false, fmt.Errorf("format secondary expiry for backfill: %w", formatErr)
		}
		secret.Annotations[keyutil.SecondaryExpiryAnnotation] = secondaryValue
		changed = true
	}
	return metadata, changed, nil
}

func secondaryExpiryFromStatus(apiKey *v1alpha1.APIKey) (*time.Time, bool) {
	if apiKey.Status.Secondary == nil || apiKey.Status.Secondary.ExpiresAt == nil || apiKey.Status.Secondary.ExpiresAt.IsZero() {
		return nil, false
	}
	expiresAt := normalizeLifecycleDeadline(apiKey.Status.Secondary.ExpiresAt.Time)
	return &expiresAt, true
}

func expiryMetadataForUnownedSecret(apiKey *v1alpha1.APIKey, secret *corev1.Secret) (*secretExpiryMetadata, error) {
	metadata := &secretExpiryMetadata{}
	primary, err := keyutil.ParsePrimaryExpiry(secret.Annotations)
	if err == nil {
		if !primary.IsZero() {
			metadata.Primary = &primary
		}
	} else if fallback, found, statusErr := primaryExpiryFromStatus(apiKey); statusErr != nil {
		return nil, statusErr
	} else if found {
		metadata.Primary = fallback
	}

	if len(secret.Data[keyutil.SecondarySecretKey]) > 0 {
		if secondary, parseErr := keyutil.ParseSecondaryExpiry(secret.Annotations); parseErr == nil {
			secondary = capExpiry(secondary, metadata.Primary)
			metadata.Secondary = &secondary
		} else if fallback, found := secondaryExpiryFromStatus(apiKey); found {
			secondary := capExpiry(*fallback, metadata.Primary)
			metadata.Secondary = &secondary
		}
	}
	return metadata, nil
}

// clearSecretRef resets the secret reference so a new Secret will be created on next reconcile.
func (r *APIKeyReconciler) clearSecretRef(ctx context.Context, apiKey *v1alpha1.APIKey) error {
	return r.updateStatusWithRetry(ctx, apiKey, func(obj *v1alpha1.APIKey) {
		obj.Status.SecretRef = nil
		obj.Status.Secondary = nil
	})
}

// isRotationDue checks whether a scheduled rotation should execute now.
// It returns true only when rotateAt is in the past and the controller has not
// yet completed a rotation for that schedule (lastRotatedAt < rotateAt).
func (r *APIKeyReconciler) isRotationDue(apiKey *v1alpha1.APIKey, secret *corev1.Secret, now time.Time) bool {
	if apiKey.Spec.Rotation == nil || apiKey.Spec.Rotation.RotateAt == nil {
		return false
	}
	rotateAt := normalizeLifecycleDeadline(apiKey.Spec.Rotation.RotateAt.Time)
	if rotateAt.After(now) {
		return false
	}

	requestValue, err := keyutil.FormatExpiry(rotateAt)
	if err == nil {
		if appliedRequest, ok := secret.Annotations[keyutil.RotationRequestAnnotation]; ok {
			return appliedRequest != requestValue
		}
	}

	// Secrets created by older operators have no request marker, so retain the
	// legacy status fallback until the first rotation writes one atomically.
	if apiKey.Status.LastRotatedAt != nil && !apiKey.Status.LastRotatedAt.Time.Before(rotateAt) {
		return false
	}
	return true
}

// cleanupExpiredSecondaryKey removes the secondary key from the Secret
// if its grace period has expired.
func (r *APIKeyReconciler) cleanupExpiredSecondaryKey(ctx context.Context, apiKey *v1alpha1.APIKey, secret *corev1.Secret, secondaryExpiresAt *time.Time, now time.Time) error {
	if secondaryExpiresAt == nil || now.Before(*secondaryExpiresAt) {
		return nil
	}
	logger := log.FromContext(ctx)
	secondaryKeyID := keyutil.KeyIDFromHash(keyutil.HashKey(string(secret.Data[keyutil.SecondarySecretKey])))
	logger.Info("Secondary key grace period expired, removing", "namespace", apiKey.Namespace, "secondaryKeyID", secondaryKeyID, "primaryKeyID", primaryKeyID(apiKey))
	if err := r.updateSecretWithRetry(ctx, secret, apiKey, func(s *corev1.Secret) (bool, error) {
		changed := false
		if _, ok := s.Data[keyutil.SecondarySecretKey]; ok {
			delete(s.Data, keyutil.SecondarySecretKey)
			changed = true
		}
		for _, annotation := range []string{keyutil.SecondaryExpiryAnnotation, keyutil.SecondaryHashAnnotation} {
			if _, ok := s.Annotations[annotation]; ok {
				delete(s.Annotations, annotation)
				changed = true
			}
		}
		return changed, nil
	}); err != nil {
		return err
	}
	if err := r.updateStatusWithRetry(ctx, apiKey, func(obj *v1alpha1.APIKey) {
		obj.Status.Secondary = nil
	}); err != nil {
		return err
	}
	r.emitEventf(apiKey, corev1.EventTypeNormal, eventReasonSecondaryKeyRemoved, "Removed expired secondary API key from Secret %q", secret.Name)
	return nil
}

// syncKeyIndex was moved to the authz-plugin component (internal/indexsync).

func keyStatusForSecretKey(rawKey []byte, expiresAt *time.Time) *v1alpha1.KeyStatus {
	if len(rawKey) == 0 || expiresAt == nil {
		return nil
	}
	normalizedExpiry := normalizeLifecycleDeadline(*expiresAt)
	return &v1alpha1.KeyStatus{
		KeyID:     keyutil.KeyIDFromHash(keyutil.HashKey(string(rawKey))),
		ExpiresAt: &metav1.Time{Time: normalizedExpiry},
	}
}

// updateActiveStatus sets the APIKey status to Active from authoritative Secret metadata.
func (r *APIKeyReconciler) updateActiveStatus(ctx context.Context, apiKey *v1alpha1.APIKey, keyID string, expiresAt *time.Time, secondary *v1alpha1.KeyStatus, now time.Time) error {
	return r.updateStatusWithRetry(ctx, apiKey, func(obj *v1alpha1.APIKey) {
		metaNow := metav1.NewTime(now)
		obj.Status.Primary = &v1alpha1.KeyStatus{KeyID: keyID}
		if normalizedExpiry := normalizeOptionalDeadline(expiresAt); normalizedExpiry != nil {
			obj.Status.Primary.ExpiresAt = &metav1.Time{Time: *normalizedExpiry}
		}
		obj.Status.Secondary = secondary
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

// computeRequeueAfter returns the earliest requeue duration based on
// key expiry, grace period expiry, and scheduled rotation.
func (r *APIKeyReconciler) computeRequeueAfter(apiKey *v1alpha1.APIKey, expiresAt, secondaryExpiresAt *time.Time, now time.Time) time.Duration {
	var earliest time.Duration
	if expiresAt != nil {
		if d := expiresAt.Sub(now); d > 0 {
			earliest = d
		}
	}
	if secondaryExpiresAt != nil {
		if d := secondaryExpiresAt.Sub(now); d > 0 && (earliest == 0 || d < earliest) {
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

// handleRotation performs key rotation: generates a new key, keeps the old key
// valid during the grace period, and updates the Secret and status accordingly.
func (r *APIKeyReconciler) handleRotation(ctx context.Context, apiKey *v1alpha1.APIKey, secret *corev1.Secret, prefix string, now time.Time) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	newExpiresAt, err := replacementPrimaryExpiry(apiKey, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	primaryExpiryValue, err := keyutil.FormatPrimaryExpiry(newExpiresAt)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("format rotated primary expiry: %w", err)
	}
	rotationRequestValue, err := keyutil.FormatExpiry(normalizeLifecycleDeadline(apiKey.Spec.Rotation.RotateAt.Time))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("format rotation request: %w", err)
	}
	rotationTimeValue, err := keyutil.FormatExpiry(now)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("format rotation time: %w", err)
	}

	result, err := keyutil.GenerateAPIKey(prefix)
	if err != nil {
		r.emitEventWithAction(apiKey, corev1.EventTypeWarning, eventReasonRotationFailed, eventActionGenerateReplacementKey, "Failed to generate replacement API key")
		return ctrl.Result{}, fmt.Errorf("failed to generate API key for rotation: %w", err)
	}

	var secondaryExpiresAt time.Time
	var oldPrimaryHash string
	// Re-read both key bytes and their bound deadline inside every conflict retry;
	// status must describe the exact Secret generation that was mutated.
	if err := r.updateSecretWithRetry(ctx, secret, apiKey, func(s *corev1.Secret) (bool, error) {
		if len(s.Data) == 0 {
			return false, fmt.Errorf("secret %s/%s data is empty, cannot rotate", s.Namespace, s.Name)
		}
		primaryKey, ok := s.Data[keyutil.SecretKey]
		if !ok || len(primaryKey) == 0 {
			return false, fmt.Errorf("secret %s/%s primary API key missing or empty, cannot rotate", s.Namespace, s.Name)
		}
		oldPrimaryHash = keyutil.HashKey(string(primaryKey))
		if s.Annotations[keyutil.PrimaryHashAnnotation] != oldPrimaryHash {
			return false, errSecretKeyBindingMismatch
		}
		oldPrimaryExpiry, parseErr := keyutil.ParsePrimaryExpiry(s.Annotations)
		if parseErr != nil {
			return false, fmt.Errorf("parse current primary expiry before rotation: %w", parseErr)
		}
		var oldPrimaryDeadline *time.Time
		if !oldPrimaryExpiry.IsZero() {
			oldPrimaryDeadline = &oldPrimaryExpiry
		}

		secondaryExpiresAt = normalizeLifecycleDeadline(now.Add(r.gracePeriod(apiKey)))
		secondaryExpiresAt = capExpiry(secondaryExpiresAt, oldPrimaryDeadline)
		secondaryExpiresAt = normalizeLifecycleDeadline(capExpiry(secondaryExpiresAt, newExpiresAt))
		secondaryExpiryValue, formatErr := keyutil.FormatExpiry(secondaryExpiresAt)
		if formatErr != nil {
			return false, fmt.Errorf("format rotated secondary expiry: %w", formatErr)
		}

		s.Data[keyutil.SecondarySecretKey] = primaryKey
		s.Data[keyutil.SecretKey] = []byte(result.RawKey)
		s.Annotations[keyutil.PrimaryExpiryAnnotation] = primaryExpiryValue
		s.Annotations[keyutil.SecondaryExpiryAnnotation] = secondaryExpiryValue
		s.Annotations[keyutil.PrimaryHashAnnotation] = result.Hash
		s.Annotations[keyutil.SecondaryHashAnnotation] = oldPrimaryHash
		s.Annotations[keyutil.RotationRequestAnnotation] = rotationRequestValue
		s.Annotations[keyutil.LastRotatedAtAnnotation] = rotationTimeValue
		return true, nil
	}); err != nil {
		r.emitEventfWithAction(apiKey, corev1.EventTypeWarning, eventReasonRotationFailed, eventActionRotateSecret, "Failed to update Secret %q with rotated API key material", secret.Name)
		return ctrl.Result{}, fmt.Errorf("failed to update secret for rotation: %w", err)
	}

	newKeyID := keyutil.KeyIDFromHash(result.Hash)
	oldPrimaryKeyID := keyutil.KeyIDFromHash(oldPrimaryHash)
	metaNow := metav1.NewTime(now)

	if err := r.updateStatusWithRetry(ctx, apiKey, func(obj *v1alpha1.APIKey) {
		obj.Status.Primary = &v1alpha1.KeyStatus{KeyID: newKeyID}
		if newExpiresAt != nil {
			obj.Status.Primary.ExpiresAt = &metav1.Time{Time: *newExpiresAt}
		}
		obj.Status.Phase = v1alpha1.PhaseActive
		obj.Status.LastSyncedAt = &metaNow
		obj.Status.LastRotatedAt = &metaNow
		obj.Status.Secondary = &v1alpha1.KeyStatus{
			KeyID:     oldPrimaryKeyID,
			ExpiresAt: &metav1.Time{Time: secondaryExpiresAt},
		}
		meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Rotated",
			Message:            fmt.Sprintf("Key rotated, secondary key valid until %s", secondaryExpiresAt.Format(time.RFC3339)),
			LastTransitionTime: metaNow,
		})
	}); err != nil {
		r.emitEventfWithAction(apiKey, corev1.EventTypeWarning, eventReasonRotationFailed, eventActionPersistRotationStatus, "Failed to persist APIKey status after rotating Secret %q", secret.Name)
		return ctrl.Result{}, err
	}

	r.emitEventf(apiKey, corev1.EventTypeNormal, eventReasonRotated, "Key rotated, secondary key valid until %s", secondaryExpiresAt.Format(time.RFC3339))
	logger.Info("APIKey rotated",
		"namespace", apiKey.Namespace,
		"newKeyID", newKeyID,
		"secondaryKeyID", oldPrimaryKeyID,
		"gracePeriodEnds", secondaryExpiresAt.Format(time.RFC3339),
	)

	if requeueAfter := secondaryExpiresAt.Sub(now); requeueAfter > 0 {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	return ctrl.Result{RequeueAfter: time.Second}, nil
}

func rotationAlreadyApplied(apiKey *v1alpha1.APIKey, secret *corev1.Secret) bool {
	if apiKey.Status.Primary == nil || apiKey.Status.Primary.KeyID == "" {
		return false
	}
	primaryRaw := secret.Data[keyutil.SecretKey]
	secondaryRaw := secret.Data[keyutil.SecondarySecretKey]
	if len(primaryRaw) == 0 || len(secondaryRaw) == 0 {
		return false
	}
	primaryKeyID := keyutil.KeyIDFromHash(keyutil.HashKey(string(primaryRaw)))
	secondaryKeyID := keyutil.KeyIDFromHash(keyutil.HashKey(string(secondaryRaw)))
	return primaryKeyID != apiKey.Status.Primary.KeyID && secondaryKeyID == apiKey.Status.Primary.KeyID
}

func (r *APIKeyReconciler) recoverRotationStatus(ctx context.Context, apiKey *v1alpha1.APIKey, secret *corev1.Secret, metadata *secretExpiryMetadata, now time.Time) (ctrl.Result, error) {
	primaryKeyID := keyutil.KeyIDFromHash(keyutil.HashKey(string(secret.Data[keyutil.SecretKey])))
	secondaryStatus := keyStatusForSecretKey(secret.Data[keyutil.SecondarySecretKey], metadata.Secondary)
	if secondaryStatus == nil {
		return ctrl.Result{}, fmt.Errorf("cannot recover rotation status without authoritative secondary metadata")
	}
	rotatedAt, err := keyutil.ParseExpiry(secret.Annotations[keyutil.LastRotatedAtAnnotation], false)
	if err != nil || rotatedAt.After(now) {
		return ctrl.Result{}, fmt.Errorf("cannot recover rotation status without a valid persisted rotation time")
	}
	metaNow := metav1.NewTime(now)
	metaRotatedAt := metav1.NewTime(rotatedAt)
	if err := r.updateStatusWithRetry(ctx, apiKey, func(obj *v1alpha1.APIKey) {
		obj.Status.Primary = &v1alpha1.KeyStatus{KeyID: primaryKeyID}
		if normalizedExpiry := normalizeOptionalDeadline(metadata.Primary); normalizedExpiry != nil {
			obj.Status.Primary.ExpiresAt = &metav1.Time{Time: *normalizedExpiry}
		}
		obj.Status.Secondary = secondaryStatus
		obj.Status.Phase = v1alpha1.PhaseActive
		obj.Status.LastSyncedAt = &metaNow
		obj.Status.LastRotatedAt = &metaRotatedAt
		meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Rotated",
			Message:            fmt.Sprintf("Recovered completed key rotation, secondary key valid until %s", metadata.Secondary.Format(time.RFC3339)),
			LastTransitionTime: metaNow,
		})
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.emitEventf(apiKey, corev1.EventTypeNormal, eventReasonRotated, "Recovered completed key rotation, secondary key valid until %s", metadata.Secondary.Format(time.RFC3339))
	if requeueAfter := r.computeRequeueAfter(apiKey, metadata.Primary, metadata.Secondary, now); requeueAfter > 0 {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	return ctrl.Result{RequeueAfter: time.Second}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *APIKeyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.APIKey{}).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.secretToAPIKey),
		).
		Complete(r)
}

// primaryKeyID returns the primary key ID from status, or "<none>" if not set.
func primaryKeyID(apiKey *v1alpha1.APIKey) string {
	if apiKey.Status.Primary != nil && apiKey.Status.Primary.KeyID != "" {
		return apiKey.Status.Primary.KeyID
	}
	return "<none>"
}

func (r *APIKeyReconciler) secretToAPIKey(_ context.Context, obj client.Object) []reconcile.Request {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.APIVersion == v1alpha1.GroupVersion.String() && ref.Kind == "APIKey" {
			return []reconcile.Request{
				{NamespacedName: types.NamespacedName{
					Namespace: obj.GetNamespace(),
					Name:      ref.Name,
				}},
			}
		}
	}
	return nil
}
