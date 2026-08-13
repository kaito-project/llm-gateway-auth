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

package azure

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

// buildFilterMetadata is a test helper that constructs the filter metadata map
// with claims nested under the "payload" key within the jwt_authn filter,
// matching how Istio's RequestAuthentication populates filter metadata.
func buildFilterMetadata(t *testing.T, claims map[string]interface{}) map[string]*structpb.Struct {
	t.Helper()
	wrapper := map[string]interface{}{
		"payload": claims,
	}
	s, err := structpb.NewStruct(wrapper)
	require.NoError(t, err, "failed to build struct")
	return map[string]*structpb.Struct{
		jwtAuthnFilterName: s,
	}
}

func Test_claimsFromFilterMetadata_FullClaims(t *testing.T) {
	t.Parallel()
	md := buildFilterMetadata(t, map[string]interface{}{
		"oid":                "abc-123",
		"preferred_username": "alice@contoso.com",
		"name":               "Alice",
		"tid":                "tenant-456",
		"sub":                "sub-789",
		"groups":             []interface{}{"group-a", "group-b"},
	})

	claims, err := claimsFromFilterMetadata(md)
	require.NoError(t, err)

	assert.Equal(t, "abc-123", claims.OID)
	assert.Equal(t, "alice@contoso.com", claims.PreferredUsername)
	assert.Equal(t, "Alice", claims.Name)
	assert.Equal(t, "tenant-456", claims.TenantID)
	assert.Equal(t, "sub-789", claims.Subject)
	assert.Equal(t, []string{"group-a", "group-b"}, claims.Groups)
}

func Test_claimsFromFilterMetadata_OIDOnly(t *testing.T) {
	t.Parallel()
	md := buildFilterMetadata(t, map[string]interface{}{
		"oid": "abc-123",
	})

	claims, err := claimsFromFilterMetadata(md)
	require.NoError(t, err)

	assert.Equal(t, "abc-123", claims.OID)
	assert.Empty(t, claims.PreferredUsername)
	assert.Nil(t, claims.Groups)
}

func Test_claimsFromFilterMetadata_SubFallback(t *testing.T) {
	t.Parallel()
	md := buildFilterMetadata(t, map[string]interface{}{
		"sub": "sub-only",
	})

	claims, err := claimsFromFilterMetadata(md)
	require.NoError(t, err)

	assert.Equal(t, "sub-only", claims.Subject)
	assert.Empty(t, claims.OID)
}

func Test_claimsFromFilterMetadata_NilMap(t *testing.T) {
	t.Parallel()
	_, err := claimsFromFilterMetadata(nil)
	assert.ErrorIs(t, err, errNoFilterMetadata)
}

func Test_claimsFromFilterMetadata_WrongKey(t *testing.T) {
	t.Parallel()
	s, _ := structpb.NewStruct(map[string]interface{}{"oid": "abc"})
	md := map[string]*structpb.Struct{
		"wrong.filter.name": s,
	}
	_, err := claimsFromFilterMetadata(md)
	assert.ErrorIs(t, err, errNoFilterMetadata)
}

func Test_claimsFromFilterMetadata_EmptyStruct(t *testing.T) {
	t.Parallel()
	s, _ := structpb.NewStruct(map[string]interface{}{})
	md := map[string]*structpb.Struct{
		jwtAuthnFilterName: s,
	}
	_, err := claimsFromFilterMetadata(md)
	assert.ErrorIs(t, err, errNoFilterMetadata)
}

func Test_claimsFromFilterMetadata_NoOIDOrSub(t *testing.T) {
	t.Parallel()
	md := buildFilterMetadata(t, map[string]interface{}{
		"preferred_username": "alice@contoso.com",
		"tid":                "tenant-456",
	})

	_, err := claimsFromFilterMetadata(md)
	assert.ErrorIs(t, err, errNoFilterMetadata)
}

func Test_toKubernetesSubject_OIDPreferred(t *testing.T) {
	t.Parallel()
	claims := &azureADClaims{OID: "oid-123", Subject: "sub-456"}
	user, _, _ := toKubernetesSubject(claims)
	assert.Equal(t, "oid-123", user)
}

func Test_toKubernetesSubject_SubFallback(t *testing.T) {
	t.Parallel()
	claims := &azureADClaims{Subject: "sub-456"}
	user, _, _ := toKubernetesSubject(claims)
	assert.Equal(t, "sub-456", user)
}

func Test_toKubernetesSubject_Groups(t *testing.T) {
	t.Parallel()
	claims := &azureADClaims{OID: "oid-123", Groups: []string{"g1", "g2"}}
	_, groups, _ := toKubernetesSubject(claims)
	assert.Equal(t, []string{"g1", "g2"}, groups)
}

func Test_toKubernetesSubject_ExtraFields(t *testing.T) {
	t.Parallel()
	claims := &azureADClaims{
		OID:               "oid-123",
		PreferredUsername: "alice@contoso.com",
		TenantID:          "tenant-456",
	}
	_, _, extra := toKubernetesSubject(claims)

	assert.Equal(t, []string{"oid-123"}, []string(extra["oid"]))
	assert.Equal(t, []string{"alice@contoso.com"}, []string(extra["preferred_username"]))
	assert.Equal(t, []string{"tenant-456"}, []string(extra["tid"]))
}

func Test_toKubernetesSubject_EmptyOptionalFields(t *testing.T) {
	t.Parallel()
	claims := &azureADClaims{OID: "oid-123"}
	_, _, extra := toKubernetesSubject(claims)

	assert.Contains(t, extra, "oid")
	assert.NotContains(t, extra, "preferred_username")
	assert.NotContains(t, extra, "tid")
}
