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

package apikey

import (
	"context"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc/codes"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/kaito-project/llm-gateway-auth/internal/keyindex"
	"github.com/kaito-project/llm-gateway-auth/internal/keyutil"
)

func newTestServer() (*Server, *keyindex.Index) {
	return newTestServerAt(time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC))
}

func newTestServerAt(now time.Time) (*Server, *keyindex.Index) {
	idx := keyindex.New()
	logger, _ := zap.NewDevelopment()
	return NewServer(idx, logger, clocktesting.NewFakeClock(now)), idx
}

func makeCheckRequest(authHeader string) *authv3.CheckRequest {
	return makeCheckRequestFull(authHeader, "", "")
}

func makeCheckRequestWithGateway(authHeader, gatewayNamespace string) *authv3.CheckRequest {
	return makeCheckRequestFull(authHeader, gatewayNamespace, "")
}

func makeCheckRequestWithHost(authHeader, host string) *authv3.CheckRequest {
	return makeCheckRequestFull(authHeader, "", host)
}

func makeCheckRequestFull(authHeader, gatewayNamespace, host string) *authv3.CheckRequest {
	req := &authv3.CheckRequest{
		Attributes: &authv3.AttributeContext{
			Request: &authv3.AttributeContext_Request{
				Http: &authv3.AttributeContext_HttpRequest{
					Headers: map[string]string{
						authorizationHeader: authHeader,
					},
					Host: host,
					Path: "/v1/chat/completions",
				},
			},
			Source: &authv3.AttributeContext_Peer{
				Address: &corev3.Address{
					Address: &corev3.Address_SocketAddress{
						SocketAddress: &corev3.SocketAddress{
							Address: "127.0.0.1",
						},
					},
				},
			},
		},
	}
	if gatewayNamespace != "" {
		req.Attributes.ContextExtensions = map[string]string{
			"gateway-namespace": gatewayNamespace,
		}
	}
	return req
}

func TestCheck_MissingAuth(t *testing.T) {
	srv, _ := newTestServer()
	resp, err := srv.Check(context.Background(), makeCheckRequestWithGateway("", "default"))
	require.NoError(t, err)
	assert.NotNil(t, resp.GetDeniedResponse(), "expected denied response for missing auth")
	assert.Equal(t, "missing API key credentials: expected Authorization, API-Key, or X-API-Key header", resp.GetStatus().GetMessage())
}

func TestCheck_InvalidFormat(t *testing.T) {
	srv, _ := newTestServer()
	resp, err := srv.Check(context.Background(), makeCheckRequestWithGateway("Basic invalid-key", "default"))
	require.NoError(t, err)
	assert.NotNil(t, resp.GetDeniedResponse(), "expected denied response for invalid format")
	assert.Equal(t, "malformed Authorization header", resp.GetStatus().GetMessage())
}

func TestCheck_UnknownKey(t *testing.T) {
	srv, _ := newTestServer()
	result, _ := keyutil.GenerateAPIKey("sk-")
	resp, err := srv.Check(context.Background(), makeCheckRequestWithGateway("Bearer "+result.RawKey, "default"))
	require.NoError(t, err)
	assert.NotNil(t, resp.GetDeniedResponse(), "expected denied response for unknown key")
	assert.Equal(t, "invalid API key in Authorization header", resp.GetStatus().GetMessage())
}

func TestCheck_ExpiryBoundary(t *testing.T) {
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		expiresAt time.Time
		allowed   bool
	}{
		{name: "finite before expiry", expiresAt: now.Add(time.Nanosecond), allowed: true},
		{name: "finite at expiry", expiresAt: now, allowed: false},
		{name: "finite after expiry", expiresAt: now.Add(-time.Nanosecond), allowed: false},
		{name: "explicit never record", expiresAt: time.Time{}, allowed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			core, logs := observer.New(zap.InfoLevel)
			idx := keyindex.New()
			srv := NewServer(idx, zap.New(core), clocktesting.NewFakeClock(now))
			result, err := keyutil.GenerateAPIKey("sk-")
			require.NoError(t, err)
			idx.Upsert(result.Hash, keyindex.TenantRecord{
				Namespace: "team-alpha",
				KeyID:     result.KeyID,
				ExpiresAt: tt.expiresAt,
			})

			resp, err := srv.Check(context.Background(), makeCheckRequestWithGateway("Bearer "+result.RawKey, "team-alpha"))
			require.NoError(t, err)
			if tt.allowed {
				assert.NotNil(t, resp.GetOkResponse())
				return
			}
			assert.NotNil(t, resp.GetDeniedResponse())
			assert.Equal(t, int32(codes.Unauthenticated), resp.GetStatus().GetCode())
			assert.Equal(t, "invalid API key in Authorization header", resp.GetStatus().GetMessage())
			require.Len(t, logs.All(), 1)
			assert.Equal(t, "deny_expired_key", logs.All()[0].ContextMap()["result"])
		})
	}
}

func TestCheck_ExpiredHigherPrecedenceHeaderDoesNotFallBack(t *testing.T) {
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	srv, idx := newTestServerAt(now)
	expired, err := keyutil.GenerateAPIKey("sk-")
	require.NoError(t, err)
	valid, err := keyutil.GenerateAPIKey("sk-")
	require.NoError(t, err)
	idx.SetNamespaceKeys("team-alpha", map[string]keyindex.TenantRecord{
		expired.Hash: {Namespace: "team-alpha", KeyID: expired.KeyID, ExpiresAt: now},
		valid.Hash:   {Namespace: "team-alpha", KeyID: valid.KeyID},
	})

	req := makeCheckRequestWithGateway("Bearer "+expired.RawKey, "team-alpha")
	req.GetAttributes().GetRequest().GetHttp().GetHeaders()[apiKeyHeader] = valid.RawKey
	resp, err := srv.Check(context.Background(), req)
	require.NoError(t, err)
	assert.NotNil(t, resp.GetDeniedResponse())
	assert.Equal(t, "invalid API key in Authorization header", resp.GetStatus().GetMessage())
}

func TestCheck_InvalidAuthorizationDoesNotFallBack(t *testing.T) {
	srv, idx := newTestServer()
	result, _ := keyutil.GenerateAPIKey("sk-")
	idx.Upsert(result.Hash, keyindex.TenantRecord{
		Namespace: "default",
		KeyID:     result.KeyID,
	})

	req := makeCheckRequestWithGateway("Bearer invalid-key", "default")
	req.GetAttributes().GetRequest().GetHttp().GetHeaders()[apiKeyHeader] = result.RawKey
	req.GetAttributes().GetRequest().GetHttp().GetHeaders()[xAPIKeyHeader] = result.RawKey
	resp, err := srv.Check(context.Background(), req)
	require.NoError(t, err)
	assert.NotNil(t, resp.GetDeniedResponse(), "expected denied response for invalid Authorization key")
	assert.Equal(t, "invalid API key in Authorization header", resp.GetStatus().GetMessage())
}

func TestCheck_InvalidAPIKeyDoesNotFallBackToXAPIKey(t *testing.T) {
	srv, idx := newTestServer()
	result, _ := keyutil.GenerateAPIKey("sk-")
	idx.Upsert(result.Hash, keyindex.TenantRecord{
		Namespace: "default",
		KeyID:     result.KeyID,
	})

	req := makeCheckRequestWithGateway("", "default")
	req.GetAttributes().GetRequest().GetHttp().GetHeaders()[apiKeyHeader] = "invalid-key"
	req.GetAttributes().GetRequest().GetHttp().GetHeaders()[xAPIKeyHeader] = result.RawKey
	resp, err := srv.Check(context.Background(), req)
	require.NoError(t, err)
	assert.NotNil(t, resp.GetDeniedResponse(), "expected denied response for invalid API-Key")
	assert.Equal(t, "invalid API key in API-Key header", resp.GetStatus().GetMessage())
}

func TestCheck_ValidKey(t *testing.T) {
	srv, idx := newTestServer()
	result, _ := keyutil.GenerateAPIKey("sk-")
	idx.Upsert(result.Hash, keyindex.TenantRecord{
		Namespace: "team-alpha",
		KeyID:     result.KeyID,
	})

	resp, err := srv.Check(context.Background(), makeCheckRequestWithGateway("Bearer "+result.RawKey, "team-alpha"))
	require.NoError(t, err)

	okResp := resp.GetOkResponse()
	require.NotNil(t, okResp, "expected OK response")

	headers := make(map[string]string)
	for _, h := range okResp.GetHeaders() {
		headers[h.GetHeader().GetKey()] = h.GetHeader().GetValue()
	}
	assert.Equal(t, "team-alpha", headers["x-tenant-namespace"])
	assert.Equal(t, result.KeyID, headers["x-key-id"])
}

func TestCheck_ValidAPIKey(t *testing.T) {
	srv, idx := newTestServer()
	result, _ := keyutil.GenerateAPIKey("sk-")
	idx.Upsert(result.Hash, keyindex.TenantRecord{
		Namespace: "team-alpha",
		KeyID:     result.KeyID,
	})

	req := makeCheckRequestWithGateway("", "team-alpha")
	req.GetAttributes().GetRequest().GetHttp().GetHeaders()[apiKeyHeader] = result.RawKey
	resp, err := srv.Check(context.Background(), req)
	require.NoError(t, err)

	okResp := resp.GetOkResponse()
	require.NotNil(t, okResp, "expected OK response")

	headers := make(map[string]string)
	for _, h := range okResp.GetHeaders() {
		headers[h.GetHeader().GetKey()] = h.GetHeader().GetValue()
	}
	assert.Equal(t, "team-alpha", headers["x-tenant-namespace"])
	assert.Equal(t, result.KeyID, headers["x-key-id"])
}

func TestCheck_ValidXAPIKey(t *testing.T) {
	srv, idx := newTestServer()
	result, _ := keyutil.GenerateAPIKey("sk-")
	idx.Upsert(result.Hash, keyindex.TenantRecord{
		Namespace: "team-alpha",
		KeyID:     result.KeyID,
	})

	req := makeCheckRequestWithGateway("", "team-alpha")
	req.GetAttributes().GetRequest().GetHttp().GetHeaders()[xAPIKeyHeader] = result.RawKey
	resp, err := srv.Check(context.Background(), req)
	require.NoError(t, err)

	okResp := resp.GetOkResponse()
	require.NotNil(t, okResp, "expected OK response")

	headers := make(map[string]string)
	for _, h := range okResp.GetHeaders() {
		headers[h.GetHeader().GetKey()] = h.GetHeader().GetValue()
	}
	assert.Equal(t, "team-alpha", headers["x-tenant-namespace"])
	assert.Equal(t, result.KeyID, headers["x-key-id"])
}

func TestCheck_HeaderPrecedence(t *testing.T) {
	srv, idx := newTestServer()
	authResult, _ := keyutil.GenerateAPIKey("sk-")
	apiKeyResult, _ := keyutil.GenerateAPIKey("sk-")
	xAPIKeyResult, _ := keyutil.GenerateAPIKey("sk-")
	idx.SetNamespaceKeys("team-alpha", map[string]keyindex.TenantRecord{
		authResult.Hash: {
			Namespace: "team-alpha",
			KeyID:     authResult.KeyID,
		},
		apiKeyResult.Hash: {
			Namespace: "team-alpha",
			KeyID:     apiKeyResult.KeyID,
		},
		xAPIKeyResult.Hash: {
			Namespace: "team-alpha",
			KeyID:     xAPIKeyResult.KeyID,
		},
	})

	tests := []struct {
		name       string
		authHeader string
		apiKey     string
		xAPIKey    string
		wantKeyID  string
	}{
		{"Authorization before API-Key and X-API-Key", "Bearer " + authResult.RawKey, apiKeyResult.RawKey, xAPIKeyResult.RawKey, authResult.KeyID},
		{"API-Key before X-API-Key", "", apiKeyResult.RawKey, xAPIKeyResult.RawKey, apiKeyResult.KeyID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := makeCheckRequestWithGateway(tt.authHeader, "team-alpha")
			req.GetAttributes().GetRequest().GetHttp().GetHeaders()[apiKeyHeader] = tt.apiKey
			req.GetAttributes().GetRequest().GetHttp().GetHeaders()[xAPIKeyHeader] = tt.xAPIKey
			resp, err := srv.Check(context.Background(), req)
			require.NoError(t, err)

			okResp := resp.GetOkResponse()
			require.NotNil(t, okResp, "expected OK response")

			headers := make(map[string]string)
			for _, h := range okResp.GetHeaders() {
				headers[h.GetHeader().GetKey()] = h.GetHeader().GetValue()
			}
			assert.Equal(t, tt.wantKeyID, headers["x-key-id"])
		})
	}
}

func TestCheck_BareAuthorizationToken(t *testing.T) {
	srv, idx := newTestServer()
	result, _ := keyutil.GenerateAPIKey("sk-")
	idx.Upsert(result.Hash, keyindex.TenantRecord{
		Namespace: "default",
		KeyID:     result.KeyID,
	})
	resp, err := srv.Check(context.Background(), makeCheckRequestWithGateway(result.RawKey, "default"))
	require.NoError(t, err)
	assert.NotNil(t, resp.GetDeniedResponse(), "expected denied response for bare Authorization token")
}

func TestExtractBearerToken(t *testing.T) {
	tests := []struct {
		header string
		want   string
	}{
		{"Bearer sk-llmgw-test", "sk-llmgw-test"},
		{"", ""},
		{"Basic abc123", ""},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, extractBearerToken(tt.header), "extractBearerToken(%q)", tt.header)
	}
}

func TestHeaderValueCaseInsensitive(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"lowercase", map[string]string{"x-api-key": "key"}, "key"},
		{"canonical", map[string]string{"X-API-Key": "key"}, "key"},
		{"mixed case", map[string]string{"x-ApI-kEy": "key"}, "key"},
		{"missing", map[string]string{"other": "value"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, headerValue(tt.headers, xAPIKeyHeader))
		})
	}
}

func TestCheck_GatewayNamespaceAllowed(t *testing.T) {
	srv, idx := newTestServer()
	result, _ := keyutil.GenerateAPIKey("sk-")
	idx.Upsert(result.Hash, keyindex.TenantRecord{
		Namespace: "team-alpha",
		KeyID:     result.KeyID,
	})

	resp, err := srv.Check(context.Background(), makeCheckRequestWithGateway("Bearer "+result.RawKey, "team-alpha"))
	require.NoError(t, err)
	assert.NotNil(t, resp.GetOkResponse(), "expected OK response when gateway namespace matches APIKey namespace")
}

func TestCheck_GatewayNamespaceDenied(t *testing.T) {
	srv, idx := newTestServer()
	result, _ := keyutil.GenerateAPIKey("sk-")
	idx.Upsert(result.Hash, keyindex.TenantRecord{
		Namespace: "team-alpha",
		KeyID:     result.KeyID,
	})

	resp, err := srv.Check(context.Background(), makeCheckRequestWithGateway("Bearer "+result.RawKey, "team-beta"))
	require.NoError(t, err)
	assert.NotNil(t, resp.GetDeniedResponse(), "expected denied response when gateway namespace does not match APIKey namespace")
}

func TestCheck_MissingGatewayNamespace(t *testing.T) {
	srv, idx := newTestServer()
	result, _ := keyutil.GenerateAPIKey("sk-")
	idx.Upsert(result.Hash, keyindex.TenantRecord{
		Namespace: "default",
		KeyID:     result.KeyID,
	})

	// No gateway-namespace in context_extensions AND no Host header → denied
	resp, err := srv.Check(context.Background(), makeCheckRequest("Bearer "+result.RawKey))
	require.NoError(t, err)
	assert.NotNil(t, resp.GetDeniedResponse(), "expected denied response when gateway-namespace is missing")
}

func TestCheck_NamespaceFromHostHeader(t *testing.T) {
	srv, idx := newTestServer()
	result, _ := keyutil.GenerateAPIKey("sk-")
	idx.Upsert(result.Hash, keyindex.TenantRecord{
		Namespace: "team-alpha",
		KeyID:     result.KeyID,
	})

	// No context_extensions but Host header has subdomain → namespace from host
	resp, err := srv.Check(context.Background(), makeCheckRequestWithHost("Bearer "+result.RawKey, "team-alpha.gw.example.com"))
	require.NoError(t, err)

	okResp := resp.GetOkResponse()
	require.NotNil(t, okResp, "expected OK response when namespace derived from Host header")

	headers := make(map[string]string)
	for _, h := range okResp.GetHeaders() {
		headers[h.GetHeader().GetKey()] = h.GetHeader().GetValue()
	}
	assert.Equal(t, "team-alpha", headers["x-tenant-namespace"])
}

func TestCheck_HostHeaderWrongNamespace(t *testing.T) {
	srv, idx := newTestServer()
	result, _ := keyutil.GenerateAPIKey("sk-")
	idx.Upsert(result.Hash, keyindex.TenantRecord{
		Namespace: "team-alpha",
		KeyID:     result.KeyID,
	})

	// Host resolves to team-beta but key belongs to team-alpha → denied
	resp, err := srv.Check(context.Background(), makeCheckRequestWithHost("Bearer "+result.RawKey, "team-beta.gw.example.com"))
	require.NoError(t, err)
	assert.NotNil(t, resp.GetDeniedResponse(), "expected denied response when Host namespace does not match key namespace")
}

func TestCheck_HostHeaderNoSubdomain(t *testing.T) {
	srv, idx := newTestServer()
	result, _ := keyutil.GenerateAPIKey("sk-")
	idx.Upsert(result.Hash, keyindex.TenantRecord{
		Namespace: "default",
		KeyID:     result.KeyID,
	})

	// Host is a bare hostname with no dots → NamespaceFromHost returns "" → denied
	resp, err := srv.Check(context.Background(), makeCheckRequestWithHost("Bearer "+result.RawKey, "localhost"))
	require.NoError(t, err)
	assert.NotNil(t, resp.GetDeniedResponse(), "expected denied response when Host has no subdomain and no context_extensions")
}

func TestCheck_ContextExtensionsTakesPriority(t *testing.T) {
	srv, idx := newTestServer()
	result, _ := keyutil.GenerateAPIKey("sk-")
	idx.Upsert(result.Hash, keyindex.TenantRecord{
		Namespace: "team-alpha",
		KeyID:     result.KeyID,
	})

	// Both context_extensions and Host set — context_extensions wins
	req := makeCheckRequestFull("Bearer "+result.RawKey, "team-alpha", "team-beta.gw.example.com")
	resp, err := srv.Check(context.Background(), req)
	require.NoError(t, err)

	okResp := resp.GetOkResponse()
	require.NotNil(t, okResp, "expected OK response when context_extensions takes priority")

	headers := make(map[string]string)
	for _, h := range okResp.GetHeaders() {
		headers[h.GetHeader().GetKey()] = h.GetHeader().GetValue()
	}
	assert.Equal(t, "team-alpha", headers["x-tenant-namespace"], "context_extensions should take priority over Host header")
}
