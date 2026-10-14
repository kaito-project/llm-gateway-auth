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

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	managedByMetadataKey = "kaito.sh/managed-by"
	managedByValue       = "llm-auth-service"
)

func hasManagedSecretAnnotation(obj metav1.Object) bool {
	return obj.GetAnnotations()[managedByMetadataKey] == managedByValue
}

func hasManagedSecretMetadata(obj metav1.Object) bool {
	return hasManagedSecretAnnotation(obj) && obj.GetLabels()[managedByMetadataKey] == managedByValue
}

func ensureManagedSecretMetadata(obj metav1.Object) bool {
	changed := false

	annotations := obj.GetAnnotations()
	if annotations[managedByMetadataKey] != managedByValue {
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[managedByMetadataKey] = managedByValue
		obj.SetAnnotations(annotations)
		changed = true
	}

	labels := obj.GetLabels()
	if labels[managedByMetadataKey] != managedByValue {
		if labels == nil {
			labels = make(map[string]string)
		}
		labels[managedByMetadataKey] = managedByValue
		obj.SetLabels(labels)
		changed = true
	}

	return changed
}
