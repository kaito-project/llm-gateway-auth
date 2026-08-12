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

package webhook

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/kaito-project/llm-gateway-auth/api/v1alpha1"
)

func TestValidateCreate(t *testing.T) {
	v := &APIKeyValidator{}
	future := metav1.NewTime(time.Now().Add(24 * time.Hour))
	past := metav1.NewTime(time.Now().Add(-1 * time.Hour))

	tests := []struct {
		name    string
		obj     *v1alpha1.APIKey
		wantErr bool
	}{
		{
			name: "valid minimal spec",
			obj: &v1alpha1.APIKey{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns"},
				Spec:       v1alpha1.APIKeySpec{Prefix: "sk-"},
			},
		},
		{
			name: "valid with expiresIn and rotation",
			obj: &v1alpha1.APIKey{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns"},
				Spec: v1alpha1.APIKeySpec{
					Prefix:    "sk-",
					ExpiresIn: "720h",
					Rotation: &v1alpha1.RotationSpec{
						RotateAt:    &future,
						GracePeriod: "24h",
					},
				},
			},
		},
		{
			name: "invalid name",
			obj: &v1alpha1.APIKey{
				ObjectMeta: metav1.ObjectMeta{Name: "not-default", Namespace: "ns"},
				Spec:       v1alpha1.APIKeySpec{Prefix: "sk-"},
			},
			wantErr: true,
		},
		{
			name: "invalid expiresIn",
			obj: &v1alpha1.APIKey{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns"},
				Spec:       v1alpha1.APIKeySpec{Prefix: "sk-", ExpiresIn: "abc"},
			},
			wantErr: true,
		},
		{
			name: "invalid gracePeriod",
			obj: &v1alpha1.APIKey{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns"},
				Spec: v1alpha1.APIKeySpec{
					Prefix: "sk-",
					Rotation: &v1alpha1.RotationSpec{
						GracePeriod: "invalid",
					},
				},
			},
			wantErr: true,
		},
		{
			name: "rotateAt in the past",
			obj: &v1alpha1.APIKey{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns"},
				Spec: v1alpha1.APIKeySpec{
					Prefix: "sk-",
					Rotation: &v1alpha1.RotationSpec{
						RotateAt: &past,
					},
				},
			},
			wantErr: true,
		},
		{
			name: "rotateAt in the future is valid",
			obj: &v1alpha1.APIKey{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns"},
				Spec: v1alpha1.APIKeySpec{
					Prefix: "sk-",
					Rotation: &v1alpha1.RotationSpec{
						RotateAt: &future,
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := v.ValidateCreate(context.Background(), tt.obj)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateUpdate(t *testing.T) {
	v := &APIKeyValidator{}
	future := metav1.NewTime(time.Now().Add(24 * time.Hour))
	laterFuture := metav1.NewTime(time.Now().Add(48 * time.Hour))

	tests := []struct {
		name    string
		oldObj  *v1alpha1.APIKey
		newObj  *v1alpha1.APIKey
		wantErr bool
	}{
		{
			name: "no-op update allowed",
			oldObj: &v1alpha1.APIKey{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns"},
				Spec:       v1alpha1.APIKeySpec{Prefix: "sk-", ExpiresIn: "720h"},
			},
			newObj: &v1alpha1.APIKey{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns"},
				Spec:       v1alpha1.APIKeySpec{Prefix: "sk-", ExpiresIn: "720h"},
			},
		},
		{
			name: "prefix change rejected",
			oldObj: &v1alpha1.APIKey{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns"},
				Spec:       v1alpha1.APIKeySpec{Prefix: "sk-"},
			},
			newObj: &v1alpha1.APIKey{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns"},
				Spec:       v1alpha1.APIKeySpec{Prefix: "myapp-"},
			},
			wantErr: true,
		},
		{
			name: "expiresIn change without rotateAt rejected",
			oldObj: &v1alpha1.APIKey{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns"},
				Spec:       v1alpha1.APIKeySpec{Prefix: "sk-", ExpiresIn: "720h"},
			},
			newObj: &v1alpha1.APIKey{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns"},
				Spec:       v1alpha1.APIKeySpec{Prefix: "sk-", ExpiresIn: "48h"},
			},
			wantErr: true,
		},
		{
			name: "expiresIn change with rotateAt allowed",
			oldObj: &v1alpha1.APIKey{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns"},
				Spec:       v1alpha1.APIKeySpec{Prefix: "sk-", ExpiresIn: "720h"},
			},
			newObj: &v1alpha1.APIKey{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns"},
				Spec: v1alpha1.APIKeySpec{
					Prefix:    "sk-",
					ExpiresIn: "48h",
					Rotation: &v1alpha1.RotationSpec{
						RotateAt: &future,
					},
				},
			},
		},
		{
			name: "expiresIn change with same rotateAt rejected",
			oldObj: &v1alpha1.APIKey{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns"},
				Spec: v1alpha1.APIKeySpec{
					Prefix:    "sk-",
					ExpiresIn: "720h",
					Rotation:  &v1alpha1.RotationSpec{RotateAt: &future},
				},
			},
			newObj: &v1alpha1.APIKey{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns"},
				Spec: v1alpha1.APIKeySpec{
					Prefix:    "sk-",
					ExpiresIn: "48h",
					Rotation:  &v1alpha1.RotationSpec{RotateAt: &future},
				},
			},
			wantErr: true,
		},
		{
			name: "expiresIn change with different rotateAt allowed",
			oldObj: &v1alpha1.APIKey{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns"},
				Spec: v1alpha1.APIKeySpec{
					Prefix:    "sk-",
					ExpiresIn: "720h",
					Rotation:  &v1alpha1.RotationSpec{RotateAt: &future},
				},
			},
			newObj: &v1alpha1.APIKey{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns"},
				Spec: v1alpha1.APIKeySpec{
					Prefix:    "sk-",
					ExpiresIn: "48h",
					Rotation:  &v1alpha1.RotationSpec{RotateAt: &laterFuture},
				},
			},
		},
		{
			name: "invalid expiresIn on update rejected",
			oldObj: &v1alpha1.APIKey{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns"},
				Spec:       v1alpha1.APIKeySpec{Prefix: "sk-"},
			},
			newObj: &v1alpha1.APIKey{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns"},
				Spec: v1alpha1.APIKeySpec{
					Prefix:    "sk-",
					ExpiresIn: "badvalue",
					Rotation:  &v1alpha1.RotationSpec{RotateAt: &future},
				},
			},
			wantErr: true,
		},
		{
			name: "adding rotateAt without changing expiresIn allowed",
			oldObj: &v1alpha1.APIKey{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns"},
				Spec:       v1alpha1.APIKeySpec{Prefix: "sk-", ExpiresIn: "720h"},
			},
			newObj: &v1alpha1.APIKey{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns"},
				Spec: v1alpha1.APIKeySpec{
					Prefix:    "sk-",
					ExpiresIn: "720h",
					Rotation:  &v1alpha1.RotationSpec{RotateAt: &future},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := v.ValidateUpdate(context.Background(), tt.oldObj, tt.newObj)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateDelete(t *testing.T) {
	v := &APIKeyValidator{}
	obj := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns"},
		Spec:       v1alpha1.APIKeySpec{Prefix: "sk-"},
	}
	_, err := v.ValidateDelete(context.Background(), obj)
	require.NoError(t, err)
}
