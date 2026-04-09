package apikey

import (
	"context"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"go.uber.org/zap"

	"github.com/kaito-project/llm-gateway-auth/internal/keyindex"
	"github.com/kaito-project/llm-gateway-auth/internal/keyutil"
)

func newTestServer() (*Server, *keyindex.Index) {
	idx := keyindex.New()
	logger, _ := zap.NewDevelopment()
	return NewServer(idx, logger), idx
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
						"authorization": authHeader,
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.GetDeniedResponse() == nil {
		t.Error("expected denied response for missing auth")
	}
}

func TestCheck_InvalidFormat(t *testing.T) {
	srv, _ := newTestServer()
	resp, err := srv.Check(context.Background(), makeCheckRequestWithGateway("Bearer invalid-key", "default"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.GetDeniedResponse() == nil {
		t.Error("expected denied response for invalid format")
	}
}

func TestCheck_UnknownKey(t *testing.T) {
	srv, _ := newTestServer()
	result, _ := keyutil.GenerateAPIKey("sk-")
	resp, err := srv.Check(context.Background(), makeCheckRequestWithGateway("Bearer "+result.RawKey, "default"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.GetDeniedResponse() == nil {
		t.Error("expected denied response for unknown key")
	}
}

func TestCheck_ValidKey(t *testing.T) {
	srv, idx := newTestServer()
	result, _ := keyutil.GenerateAPIKey("sk-")
	idx.Upsert(result.Hash, keyindex.TenantRecord{
		Namespace: "team-alpha",
		KeyID:     result.KeyID,
	})

	resp, err := srv.Check(context.Background(), makeCheckRequestWithGateway("Bearer "+result.RawKey, "team-alpha"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	okResp := resp.GetOkResponse()
	if okResp == nil {
		t.Fatal("expected OK response")
	}

	headers := make(map[string]string)
	for _, h := range okResp.GetHeaders() {
		headers[h.GetHeader().GetKey()] = h.GetHeader().GetValue()
	}
	if headers["x-tenant-namespace"] != "team-alpha" {
		t.Errorf("expected x-tenant-namespace=team-alpha, got %s", headers["x-tenant-namespace"])
	}
	if headers["x-key-id"] != result.KeyID {
		t.Errorf("expected x-key-id=%s, got %s", result.KeyID, headers["x-key-id"])
	}
}

func TestCheck_BareToken(t *testing.T) {
	srv, idx := newTestServer()
	result, _ := keyutil.GenerateAPIKey("sk-")
	idx.Upsert(result.Hash, keyindex.TenantRecord{
		Namespace: "default",
		KeyID:     result.KeyID,
	})
	resp, err := srv.Check(context.Background(), makeCheckRequestWithGateway("Bearer "+result.RawKey, "default"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.GetOkResponse() == nil {
		t.Error("expected OK response for bare token")
	}
}

func TestExtractKey(t *testing.T) {
	tests := []struct {
		header string
		want   string
	}{
		{"Bearer sk-llmgw-test", "sk-llmgw-test"},
		{"", ""},
		{"Basic abc123", ""},
	}
	for _, tt := range tests {
		got := extractKey(tt.header)
		if got != tt.want {
			t.Errorf("extractKey(%q) = %q, want %q", tt.header, got, tt.want)
		}
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.GetOkResponse() == nil {
		t.Error("expected OK response when gateway namespace matches APIKey namespace")
	}
}

func TestCheck_GatewayNamespaceDenied(t *testing.T) {
	srv, idx := newTestServer()
	result, _ := keyutil.GenerateAPIKey("sk-")
	idx.Upsert(result.Hash, keyindex.TenantRecord{
		Namespace: "team-alpha",
		KeyID:     result.KeyID,
	})

	resp, err := srv.Check(context.Background(), makeCheckRequestWithGateway("Bearer "+result.RawKey, "team-beta"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.GetDeniedResponse() == nil {
		t.Error("expected denied response when gateway namespace does not match APIKey namespace")
	}
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.GetDeniedResponse() == nil {
		t.Error("expected denied response when gateway-namespace is missing")
	}
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	okResp := resp.GetOkResponse()
	if okResp == nil {
		t.Fatal("expected OK response when namespace derived from Host header")
	}

	headers := make(map[string]string)
	for _, h := range okResp.GetHeaders() {
		headers[h.GetHeader().GetKey()] = h.GetHeader().GetValue()
	}
	if headers["x-tenant-namespace"] != "team-alpha" {
		t.Errorf("expected x-tenant-namespace=team-alpha, got %s", headers["x-tenant-namespace"])
	}
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.GetDeniedResponse() == nil {
		t.Error("expected denied response when Host namespace does not match key namespace")
	}
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.GetDeniedResponse() == nil {
		t.Error("expected denied response when Host has no subdomain and no context_extensions")
	}
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	okResp := resp.GetOkResponse()
	if okResp == nil {
		t.Fatal("expected OK response when context_extensions takes priority")
	}

	headers := make(map[string]string)
	for _, h := range okResp.GetHeaders() {
		headers[h.GetHeader().GetKey()] = h.GetHeader().GetValue()
	}
	if headers["x-tenant-namespace"] != "team-alpha" {
		t.Errorf("expected x-tenant-namespace=team-alpha (from context_extensions), got %s", headers["x-tenant-namespace"])
	}
}
