package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/kaito-project/llm-gateway-auth/api/v1alpha1"
	"github.com/kaito-project/llm-gateway-auth/internal/keyutil"
)

const finalizerName = "kaito.sh/key-cleanup"

// APIKeyReconciler reconciles APIKey objects.
type APIKeyReconciler struct {
	client.Client
}

func (r *APIKeyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var apiKey v1alpha1.APIKey
	if err := r.Get(ctx, req.NamespacedName, &apiKey); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Singleton enforcement (TODO: move to webhook later)
	if req.Name != keyutil.SingletonName {
		return ctrl.Result{}, r.markNotReady(ctx, &apiKey, "InvalidName",
			"APIKey name must be 'default' (singleton per namespace)")
	}

	if !apiKey.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &apiKey)
	}

	if err := r.ensureFinalizer(ctx, &apiKey); err != nil {
		return ctrl.Result{}, err
	}

	expiresAt, err := r.computeExpiresAt(&apiKey)
	if err != nil {
		return ctrl.Result{}, r.markNotReady(ctx, &apiKey, "InvalidExpiresIn",
			fmt.Sprintf("Invalid expiresIn duration %q: %v", apiKey.Spec.ExpiresIn, err))
	}

	if expiresAt != nil && expiresAt.Before(time.Now()) {
		return ctrl.Result{}, r.handleExpiry(ctx, &apiKey, expiresAt)
	}

	prefix := apiKey.Spec.Prefix
	if prefix == "" {
		prefix = keyutil.DefaultPrefix
	}

	// Create Secret on first run
	if apiKey.Status.SecretRef == nil {
		return r.createKeySecret(ctx, &apiKey, prefix)
	}

	// Fetch existing Secret; requeue if it was missing/corrupt and needs recreation
	secret, requeueNeeded, err := r.fetchKeySecret(ctx, &apiKey)
	if err != nil {
		return ctrl.Result{}, err
	}
	if requeueNeeded {
		return ctrl.Result{Requeue: true}, nil
	}

	if r.isRotationDue(&apiKey) {
		return r.handleRotation(ctx, &apiKey, secret, prefix)
	}

	if err := r.cleanupExpiredPreviousKey(ctx, &apiKey, secret); err != nil {
		return ctrl.Result{}, err
	}

	// Derive key identity
	rawKey := string(secret.Data[keyutil.SecretKey])
	keyID := keyutil.KeyIDFromHash(keyutil.HashKey(rawKey))

	if err := r.updateActiveStatus(ctx, &apiKey, keyID, expiresAt); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("APIKey synced", "namespace", apiKey.Namespace, "keyID", keyID)
	return ctrl.Result{RequeueAfter: r.computeRequeueAfter(&apiKey, expiresAt)}, nil
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

	logger.Info("APIKey being deleted", "namespace", apiKey.Namespace, "keyID", currentKeyID(apiKey))

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

// computeExpiresAt calculates the key expiration time based on spec.expiresIn.
// Uses last rotation time as base if available, otherwise creation time.
// Returns (nil, nil) if no expiration is configured.
func (r *APIKeyReconciler) computeExpiresAt(apiKey *v1alpha1.APIKey) (*time.Time, error) {
	if apiKey.Spec.ExpiresIn == "" {
		return nil, nil
	}
	// TODO: move to webhook validation later
	duration, err := time.ParseDuration(apiKey.Spec.ExpiresIn)
	if err != nil {
		return nil, err
	}
	baseTime := apiKey.CreationTimestamp.Time
	if apiKey.Status.LastRotatedAt != nil {
		baseTime = apiKey.Status.LastRotatedAt.Time
	}
	t := baseTime.Add(duration)
	return &t, nil
}

// handleExpiry marks the key as expired.
func (r *APIKeyReconciler) handleExpiry(ctx context.Context, apiKey *v1alpha1.APIKey, expiresAt *time.Time) error {
	logger := log.FromContext(ctx)

	if err := r.updateStatusWithRetry(ctx, apiKey, func(obj *v1alpha1.APIKey) {
		obj.Status.Phase = v1alpha1.PhaseExpired
		if obj.Status.Current == nil {
			obj.Status.Current = &v1alpha1.KeyStatus{}
		}
		obj.Status.Current.ExpiresAt = &metav1.Time{Time: *expiresAt}
		meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "Expired",
			Message:            fmt.Sprintf("Key expired at %s", expiresAt.Format(time.RFC3339)),
			LastTransitionTime: metav1.Now(),
		})
	}); err != nil {
		return err
	}
	logger.Info("APIKey expired", "namespace", apiKey.Namespace, "keyID", currentKeyID(apiKey), "expiresAt", expiresAt.Format(time.RFC3339))
	return nil
}

// createKeySecret generates a new API key, creates the Secret, and requeues.
func (r *APIKeyReconciler) createKeySecret(ctx context.Context, apiKey *v1alpha1.APIKey, prefix string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	result, err := keyutil.GenerateAPIKey(prefix)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to generate API key: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keyutil.SecretName,
			Namespace: apiKey.Namespace,
			Labels: map[string]string{
				"kaito.sh/managed-by": "llm-auth-service",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(apiKey, v1alpha1.GroupVersion.WithKind("APIKey")),
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			keyutil.SecretKey: []byte(result.RawKey),
		},
	}
	if err := r.Create(ctx, secret); err != nil {
		if !errors.IsAlreadyExists(err) {
			return ctrl.Result{}, fmt.Errorf("failed to create secret: %w", err)
		}
		// Secret already exists – verify it is owned by this APIKey before updating
		existing := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: apiKey.Namespace, Name: keyutil.SecretName}, existing); err != nil {
			return ctrl.Result{}, err
		}
		if !metav1.IsControlledBy(existing, apiKey) {
			return ctrl.Result{}, r.markNotReady(ctx, apiKey, "SecretConflict",
				fmt.Sprintf("Secret %q already exists but is not owned by this APIKey", keyutil.SecretName))
		}
		existing.Data = map[string][]byte{
			keyutil.SecretKey: []byte(result.RawKey),
		}
		if err := r.Update(ctx, existing); err != nil {
			return ctrl.Result{}, err
		}
		secret = existing
	}

	if err := r.updateStatusWithRetry(ctx, apiKey, func(obj *v1alpha1.APIKey) {
		obj.Status.SecretRef = &v1alpha1.SecretReference{
			Name: keyutil.SecretName,
		}
	}); err != nil {
		return ctrl.Result{}, err
	}
	logger.Info("Secret created for APIKey", "namespace", apiKey.Namespace, "keyID", result.KeyID)
	return ctrl.Result{Requeue: true}, nil
}

// fetchKeySecret retrieves the Secret referenced by the APIKey status.
// If the Secret is missing or has an empty key field, it clears the reference
// and signals that a requeue is needed.
func (r *APIKeyReconciler) fetchKeySecret(ctx context.Context, apiKey *v1alpha1.APIKey) (*corev1.Secret, bool, error) {
	logger := log.FromContext(ctx)
	var secret corev1.Secret
	secretNN := types.NamespacedName{
		Namespace: apiKey.Namespace,
		Name:      apiKey.Status.SecretRef.Name,
	}
	if err := r.Get(ctx, secretNN, &secret); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Secret not found, will regenerate", "namespace", apiKey.Namespace, "keyID", currentKeyID(apiKey), "secret", secretNN.Name)
			return nil, true, r.clearSecretRef(ctx, apiKey)
		}
		return nil, false, err
	}

	if rawKey, ok := secret.Data[keyutil.SecretKey]; !ok || len(rawKey) == 0 {
		logger.Info("Secret has empty key field, will regenerate", "namespace", apiKey.Namespace, "keyID", currentKeyID(apiKey), "secret", secretNN.Name)
		return nil, true, r.clearSecretRef(ctx, apiKey)
	}

	return &secret, false, nil
}

// clearSecretRef resets the secret reference so a new Secret will be created on next reconcile.
func (r *APIKeyReconciler) clearSecretRef(ctx context.Context, apiKey *v1alpha1.APIKey) error {
	return r.updateStatusWithRetry(ctx, apiKey, func(obj *v1alpha1.APIKey) {
		obj.Status.SecretRef = nil
		obj.Status.Previous = nil
	})
}

// isRotationDue checks whether a scheduled rotation should execute now.
// It returns true only when rotateAt is in the past and the controller has not
// yet completed a rotation for that schedule (lastRotatedAt < rotateAt).
func (r *APIKeyReconciler) isRotationDue(apiKey *v1alpha1.APIKey) bool {
	if apiKey.Spec.Rotation == nil || apiKey.Spec.Rotation.RotateAt == nil {
		return false
	}
	rotateAt := apiKey.Spec.Rotation.RotateAt.Time
	if rotateAt.After(time.Now()) {
		return false
	}
	// Already completed for this schedule?
	if apiKey.Status.LastRotatedAt != nil && !apiKey.Status.LastRotatedAt.Time.Before(rotateAt) {
		return false
	}
	return true
}

// cleanupExpiredPreviousKey removes the previous key from the Secret
// if its grace period has expired.
func (r *APIKeyReconciler) cleanupExpiredPreviousKey(ctx context.Context, apiKey *v1alpha1.APIKey, secret *corev1.Secret) error {
	if apiKey.Status.Previous == nil || !apiKey.Status.Previous.ExpiresAt.Time.Before(time.Now()) {
		return nil
	}
	logger := log.FromContext(ctx)
	logger.Info("Previous key grace period expired, removing", "namespace", apiKey.Namespace, "previousKeyID", apiKey.Status.Previous.KeyID, "currentKeyID", currentKeyID(apiKey))
	delete(secret.Data, keyutil.PreviousSecretKey)
	if err := r.Update(ctx, secret); err != nil {
		return err
	}
	return r.updateStatusWithRetry(ctx, apiKey, func(obj *v1alpha1.APIKey) {
		obj.Status.Previous = nil
	})
}

// syncKeyIndex was moved to the authz-plugin component (internal/indexsync).

// updateActiveStatus sets the APIKey status to Active with the current key info.
func (r *APIKeyReconciler) updateActiveStatus(ctx context.Context, apiKey *v1alpha1.APIKey, keyID string, expiresAt *time.Time) error {
	return r.updateStatusWithRetry(ctx, apiKey, func(obj *v1alpha1.APIKey) {
		now := metav1.Now()
		obj.Status.Current = &v1alpha1.KeyStatus{
			KeyID: keyID,
		}
		if expiresAt != nil {
			obj.Status.Current.ExpiresAt = &metav1.Time{Time: *expiresAt}
		}
		obj.Status.Phase = v1alpha1.PhaseActive
		obj.Status.LastSyncedAt = &now
		meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Synced",
			Message:            "Key is active",
			LastTransitionTime: metav1.Now(),
		})
	})
}

// computeRequeueAfter returns the earliest requeue duration based on
// key expiry, grace period expiry, and scheduled rotation.
func (r *APIKeyReconciler) computeRequeueAfter(apiKey *v1alpha1.APIKey, expiresAt *time.Time) time.Duration {
	var earliest time.Duration
	if expiresAt != nil {
		if d := time.Until(*expiresAt); d > 0 {
			earliest = d
		}
	}
	if apiKey.Status.Previous != nil {
		if d := time.Until(apiKey.Status.Previous.ExpiresAt.Time); d > 0 && (earliest == 0 || d < earliest) {
			earliest = d
		}
	}
	if apiKey.Spec.Rotation != nil && apiKey.Spec.Rotation.RotateAt != nil {
		if d := time.Until(apiKey.Spec.Rotation.RotateAt.Time); d > 0 && (earliest == 0 || d < earliest) {
			earliest = d
		}
	}
	return earliest
}

// handleRotation performs key rotation: generates a new key, keeps the old key
// valid during the grace period, and updates the Secret and status accordingly.
func (r *APIKeyReconciler) handleRotation(ctx context.Context, apiKey *v1alpha1.APIKey, secret *corev1.Secret, prefix string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var currentKeyID string
	if apiKey.Status.Current != nil {
		currentKeyID = apiKey.Status.Current.KeyID
	}

	// Compute grace period
	gracePeriod := 24 * time.Hour
	if apiKey.Spec.Rotation != nil && apiKey.Spec.Rotation.GracePeriod != "" {
		// TODO: move to webhook validation later
		parsed, err := time.ParseDuration(apiKey.Spec.Rotation.GracePeriod)
		if err != nil {
			if err := r.markNotReady(ctx, apiKey, "InvalidGracePeriod",
				fmt.Sprintf("Invalid gracePeriod %q: %v", apiKey.Spec.Rotation.GracePeriod, err)); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		gracePeriod = parsed
	}

	// Generate new key
	result, err := keyutil.GenerateAPIKey(prefix)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to generate API key for rotation: %w", err)
	}

	// Update Secret: move current key to previous, set new as current
	secret.Data[keyutil.PreviousSecretKey] = secret.Data[keyutil.SecretKey]
	secret.Data[keyutil.SecretKey] = []byte(result.RawKey)
	if err := r.Update(ctx, secret); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update secret for rotation: %w", err)
	}

	// Compute new key expiration
	rotationTime := time.Now()
	var newExpiresAt *time.Time
	if apiKey.Spec.ExpiresIn != "" {
		duration, err := time.ParseDuration(apiKey.Spec.ExpiresIn)
		if err == nil {
			t := rotationTime.Add(duration)
			newExpiresAt = &t
		}
	}

	newHash := keyutil.HashKey(result.RawKey)
	newKeyID := keyutil.KeyIDFromHash(newHash)
	previousExpiresAt := rotationTime.Add(gracePeriod)

	// Update status
	if err := r.updateStatusWithRetry(ctx, apiKey, func(obj *v1alpha1.APIKey) {
		metaNow := metav1.Now()
		rotatedAt := metav1.NewTime(rotationTime)
		obj.Status.Current = &v1alpha1.KeyStatus{
			KeyID: newKeyID,
		}
		if newExpiresAt != nil {
			obj.Status.Current.ExpiresAt = &metav1.Time{Time: *newExpiresAt}
		}
		obj.Status.Phase = v1alpha1.PhaseActive
		obj.Status.LastSyncedAt = &metaNow
		obj.Status.LastRotatedAt = &rotatedAt
		obj.Status.Previous = &v1alpha1.KeyStatus{
			KeyID:     currentKeyID,
			ExpiresAt: &metav1.Time{Time: previousExpiresAt},
		}
		meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Rotated",
			Message:            fmt.Sprintf("Key rotated, previous key valid until %s", previousExpiresAt.Format(time.RFC3339)),
			LastTransitionTime: metav1.Now(),
		})
	}); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("APIKey rotated",
		"namespace", apiKey.Namespace,
		"newKeyID", newKeyID,
		"previousKeyID", currentKeyID,
		"gracePeriodEnds", previousExpiresAt.Format(time.RFC3339),
	)

	return ctrl.Result{RequeueAfter: gracePeriod}, nil
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

// currentKeyID returns the current key ID from status, or "<none>" if not set.
func currentKeyID(apiKey *v1alpha1.APIKey) string {
	if apiKey.Status.Current != nil && apiKey.Status.Current.KeyID != "" {
		return apiKey.Status.Current.KeyID
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
