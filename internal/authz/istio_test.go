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

package authz

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNamespaceFromHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		host string
		want string
	}{
		{"team-alpha.mgr.example.com", "team-alpha"},
		{"team-alpha.mgr.example.com:8080", "team-alpha"},
		{"production.mgr.example.com:443", "production"},
		{"localhost", ""},
		{"", ""},
		{"localhost:8080", ""},
		{".example.com", ""},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, NamespaceFromHost(tt.host))
		})
	}
}
