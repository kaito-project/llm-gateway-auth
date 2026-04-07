package webhook

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha1 "github.com/kaito-project/llm-gateway-auth/api/v1alpha1"
	"github.com/kaito-project/llm-gateway-auth/internal/keyutil"
)

// +kubebuilder:webhook:path=/validate-kaito-sh-v1alpha1-apikey,mutating=false,failurePolicy=fail,sideEffects=None,groups=kaito.sh,resources=apikeys,verbs=create;update,versions=v1alpha1,name=vapikey.kb.io,admissionReviewVersions=v1

// APIKeyValidator implements admission.Validator for APIKey resources.
type APIKeyValidator struct{}

var _ admission.Validator[*v1alpha1.APIKey] = &APIKeyValidator{}

// ValidateCreate validates an APIKey on creation.
func (v *APIKeyValidator) ValidateCreate(_ context.Context, obj *v1alpha1.APIKey) (admission.Warnings, error) {
	var allErrs field.ErrorList

	// Singleton: name must be "default".
	if obj.Name != keyutil.SingletonName {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("metadata", "name"),
			obj.Name,
			fmt.Sprintf("must be %q (one APIKey per namespace)", keyutil.SingletonName),
		))
	}

	allErrs = append(allErrs, validateSpec(&obj.Spec)...)

	// rotateAt must be in the future on create.
	if obj.Spec.Rotation != nil && obj.Spec.Rotation.RotateAt != nil {
		if !obj.Spec.Rotation.RotateAt.After(time.Now()) {
			allErrs = append(allErrs, field.Invalid(
				field.NewPath("spec", "rotation", "rotateAt"),
				obj.Spec.Rotation.RotateAt.Format(time.RFC3339),
				"must be a time in the future",
			))
		}
	}

	if len(allErrs) > 0 {
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: v1alpha1.GroupVersion.Group, Kind: "APIKey"},
			obj.Name, allErrs,
		)
	}
	return nil, nil
}

// ValidateUpdate validates an APIKey on update.
func (v *APIKeyValidator) ValidateUpdate(_ context.Context, oldObj, newObj *v1alpha1.APIKey) (admission.Warnings, error) {
	var allErrs field.ErrorList

	allErrs = append(allErrs, validateSpec(&newObj.Spec)...)

	// Prefix is immutable after creation.
	if oldObj.Spec.Prefix != newObj.Spec.Prefix {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "prefix"),
			"field is immutable after creation",
		))
	}

	// ExpiresIn is immutable unless rotation.rotateAt is being set.
	if oldObj.Spec.ExpiresIn != newObj.Spec.ExpiresIn {
		rotateAtBeingSet := newObj.Spec.Rotation != nil && newObj.Spec.Rotation.RotateAt != nil &&
			(oldObj.Spec.Rotation == nil || oldObj.Spec.Rotation.RotateAt == nil ||
				!oldObj.Spec.Rotation.RotateAt.Time.Equal(newObj.Spec.Rotation.RotateAt.Time))
		if !rotateAtBeingSet {
			allErrs = append(allErrs, field.Forbidden(
				field.NewPath("spec", "expiresIn"),
				"field is immutable unless spec.rotation.rotateAt is also being updated",
			))
		}
	}

	if len(allErrs) > 0 {
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: v1alpha1.GroupVersion.Group, Kind: "APIKey"},
			newObj.Name, allErrs,
		)
	}
	return nil, nil
}

// ValidateDelete validates an APIKey on deletion (always allowed).
func (v *APIKeyValidator) ValidateDelete(_ context.Context, _ *v1alpha1.APIKey) (admission.Warnings, error) {
	return nil, nil
}

// validateSpec checks fields common to create and update.
func validateSpec(spec *v1alpha1.APIKeySpec) field.ErrorList {
	var errs field.ErrorList

	if spec.ExpiresIn != "" {
		if _, err := time.ParseDuration(spec.ExpiresIn); err != nil {
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "expiresIn"),
				spec.ExpiresIn,
				fmt.Sprintf("must be a valid Go duration: %v", err),
			))
		}
	}

	if spec.Rotation != nil && spec.Rotation.GracePeriod != "" {
		if _, err := time.ParseDuration(spec.Rotation.GracePeriod); err != nil {
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "rotation", "gracePeriod"),
				spec.Rotation.GracePeriod,
				fmt.Sprintf("must be a valid Go duration: %v", err),
			))
		}
	}

	return errs
}
