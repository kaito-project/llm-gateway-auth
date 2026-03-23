package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// DefaultGracePeriod is the default duration for which the previous key remains valid after rotation.
	DefaultGracePeriod = "24h"
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
	// Can be updated during rotation to change the new key's expiration.
	// Omit for no expiry.
	// +optional
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

	// GracePeriod is the duration for which the previous key remains valid after rotation.
	// Uses Go duration format (e.g. "24h", "168h" for 7 days).
	// Defaults to "24h".
	// +optional
	GracePeriod string `json:"gracePeriod,omitempty"`
}

// SecretReference references a Secret in the same namespace.
type SecretReference struct {
	// Name of the Secret.
	Name string `json:"name"`

	// Key within the Secret's data that holds the API key.
	Key string `json:"key"`
}

// APIKeyPhase represents the current phase of the APIKey.
// +kubebuilder:validation:Enum=Pending;Active;Expired
type APIKeyPhase string

const (
	PhasePending APIKeyPhase = "Pending"
	PhaseActive  APIKeyPhase = "Active"
	PhaseExpired APIKeyPhase = "Expired"
)

// PreviousKeyStatus holds information about the previous key during a grace period.
type PreviousKeyStatus struct {
	// KeyID of the previous key.
	KeyID string `json:"keyID"`

	// ExpiresAt is when the previous key stops being valid (rotation time + gracePeriod).
	ExpiresAt metav1.Time `json:"expiresAt"`
}

// APIKeyStatus defines the observed state of APIKey.
type APIKeyStatus struct {
	// KeyID is the first 4 bytes of SHA-256(raw_key) as hex, for log identification.
	// +optional
	KeyID string `json:"keyID,omitempty"`

	// Phase represents the current phase of the key.
	// +optional
	Phase APIKeyPhase `json:"phase,omitempty"`

	// ExpiresAt is the computed expiration time.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// SecretRef references the controller-managed Secret that stores the API key plaintext.
	// +optional
	SecretRef *SecretReference `json:"secretRef,omitempty"`

	// PreviousKey holds the previous key's info during the grace period after rotation.
	// +optional
	PreviousKey *PreviousKeyStatus `json:"previousKey,omitempty"`

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
// +kubebuilder:printcolumn:name="KeyID",type=string,JSONPath=`.status.keyID`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="ExpiresAt",type=string,JSONPath=`.status.expiresAt`
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

func init() {
	SchemeBuilder.Register(&APIKey{}, &APIKeyList{})
}
