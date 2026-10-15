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
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestHasManagedSecretAnnotationRequiresExactValue(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		expected    bool
	}{
		{name: "missing", annotations: nil},
		{name: "empty", annotations: map[string]string{managedByMetadataKey: ""}},
		{name: "wrong case", annotations: map[string]string{managedByMetadataKey: "LLM-Gateway-Auth-Service"}},
		{name: "exact", annotations: map[string]string{managedByMetadataKey: managedByValue}, expected: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: tt.annotations}}
			assert.Equal(t, tt.expected, hasManagedSecretAnnotation(secret))
		})
	}
}

func TestEnsureManagedSecretMetadataPreservesUnrelatedMetadata(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{
			managedByMetadataKey: "wrong",
			"example.com/note":   "keep",
		},
		Labels: map[string]string{
			managedByMetadataKey: "wrong",
			"example.com/team":   "keep",
		},
	}}

	assert.True(t, ensureManagedSecretMetadata(secret))
	assert.Equal(t, managedByValue, secret.Annotations[managedByMetadataKey])
	assert.Equal(t, managedByValue, secret.Labels[managedByMetadataKey])
	assert.Equal(t, "keep", secret.Annotations["example.com/note"])
	assert.Equal(t, "keep", secret.Labels["example.com/team"])
	assert.True(t, hasManagedSecretMetadata(secret))
	assert.False(t, ensureManagedSecretMetadata(secret), "already-correct metadata should not cause another write")
}
