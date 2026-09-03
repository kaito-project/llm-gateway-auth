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

package v1alpha1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// DefaultGracePeriod is the default duration for which the secondary key remains valid after rotation.
	// Defaults to 24 hours.
	DefaultGracePeriod time.Duration = 24 * time.Hour
)

// APIKeySpec defines the desired state of APIKey.
type APIKeySpec struct {
	// Prefix is the literal prefix of the generated API key.
	// For example, "sk-" produces keys like "sk-<random>", "sk-myapp-" produces "sk-myapp-<random>".
	// Defaults to "sk-" if not specified.
	// +optional
	// +kubebuilder:default="sk-"
	Prefix string `json:"prefix,omitempty"`

	// ExpiresIn is the duration after which the key expires, relative to creation time
	// (or last rotation time). Uses Go duration format (e.g. "720h" for 30 days).
	// This field is immutable after creation; it can only be updated when RotateAt is
	// set to trigger a new key generation.
	// Omit for no expiry.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == '' || self.matches('^([0-9]+(h|m|s))+$')",message="must be a valid Go duration (e.g. 720h, 30m, 1h30m)"
	ExpiresIn string `json:"expiresIn,omitempty"`

	// Rotation configures the key rotation behavior.
	// +optional
	Rotation *RotationSpec `json:"rotation,omitempty"`
}

// RotationSpec configures key rotation behavior.
type RotationSpec struct {
	// RotateAt schedules an automatic key rotation at the specified UTC time (RFC3339 format).
	// When the time is reached, the controller will rotate the key automatically.
	// +optional
	RotateAt *metav1.Time `json:"rotateAt,omitempty"`

	// GracePeriod is the duration for which the secondary key remains valid after rotation.
	// Uses Go duration format (e.g. "24h", "168h" for 7 days).
	// Defaults to "24h".
	//
	// This also bounds the secondary key generated when the APIKey is first
	// created: that key is removed one grace period after creation even if no
	// rotation has occurred. Raise this value to keep a standby key available
	// for longer on keys that are rotated infrequently.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == '' || self.matches('^([0-9]+(h|m|s))+$')",message="must be a valid Go duration (e.g. 720h, 30m, 1h30m)"
	GracePeriod string `json:"gracePeriod,omitempty"`
}

// SecretReference references a Secret in the same namespace.
// The referenced Secret stores both the primary key and, during a grace period, the secondary key.
type SecretReference struct {
	// Name of the Secret.
	Name string `json:"name"`
}

// APIKeyPhase represents the current phase of the APIKey.
// +kubebuilder:validation:Enum=Pending;Active;Expired
type APIKeyPhase string

const (
	PhasePending APIKeyPhase = "Pending"
	PhaseActive  APIKeyPhase = "Active"
	PhaseExpired APIKeyPhase = "Expired"
)

// KeyStatus holds information about a key.
type KeyStatus struct {
	// KeyID of the key.
	KeyID string `json:"keyID"`

	// ExpiresAt is when the key stops being valid (rotation time + gracePeriod).
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
}

// APIKeyStatus defines the observed state of APIKey.
type APIKeyStatus struct {
	// Phase represents the current phase of the key.
	// +optional
	Phase APIKeyPhase `json:"phase,omitempty"`

	// Primary holds key ID and expiration info for the active key.
	// +optional
	Primary *KeyStatus `json:"primaryKey,omitempty"`

	// Secondary holds the secondary key's info during the grace period after rotation.
	// +optional
	Secondary *KeyStatus `json:"secondaryKey,omitempty"`

	// SecretRef references the controller-managed Secret that stores the API key plaintext.
	// +optional
	SecretRef *SecretReference `json:"secretRef,omitempty"`

	// LastRotatedAt is the timestamp of the last key rotation.
	// Used as the base time for computing expiresAt after rotation.
	// +optional
	LastRotatedAt *metav1.Time `json:"lastRotatedAt,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastSyncedAt is the last time the controller successfully synced to the in-memory index.
	// +optional
	LastSyncedAt *metav1.Time `json:"lastSyncedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// The CRD ships in the chart's templates/ so that `helm upgrade` can apply schema
// changes; resource-policy=keep stops `helm uninstall` from taking the CRD — and
// every APIKey in the cluster — down with the release.
// +kubebuilder:metadata:annotations="helm.sh/resource-policy=keep"
// +kubebuilder:printcolumn:name="KeyID",type=string,JSONPath=`.status.primaryKey.keyID`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="ExpiresAt",type=string,JSONPath=`.status.primaryKey.expiresAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// APIKey is the Schema for the apikeys API.
type APIKey struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   APIKeySpec   `json:"spec,omitempty"`
	Status APIKeyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// APIKeyList contains a list of APIKey.
type APIKeyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []APIKey `json:"items"`
}
