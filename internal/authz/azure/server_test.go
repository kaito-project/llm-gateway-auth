package azure

import (
	"context"
	"errors"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/structpb"
)

// buildCheckRequest builds a CheckRequest with optional filter metadata and HTTP attributes.
func buildCheckRequest(t *testing.T, claims map[string]interface{}, host, method, path string) *authv3.CheckRequest {
	t.Helper()

	req := &authv3.CheckRequest{
		Attributes: &authv3.AttributeContext{
			Request: &authv3.AttributeContext_Request{
				Http: &authv3.AttributeContext_HttpRequest{
					Host:   host,
					Method: method,
					Path:   path,
				},
			},
		},
	}

	if claims != nil {
		wrapper := map[string]interface{}{
			"payload": claims,
		}
		s, err := structpb.NewStruct(wrapper)
		require.NoError(t, err, "failed to build struct")
		req.Attributes.MetadataContext = &corev3.Metadata{
			FilterMetadata: map[string]*structpb.Struct{
				jwtAuthnFilterName: s,
			},
		}
	}

	return req
}

func testConfig() *Config {
	return &Config{
		SARAPIGroup: "aimanager",
		SARResource: "namespaces",
		SARVerb:     "inference",
		FailOpen:    false,
	}
}

func newTestServer(t *testing.T, checker AccessChecker, cfg *Config) *AuthorizationServer {
	t.Helper()
	reg := prometheus.NewRegistry()
	return NewAuthorizationServer(checker, cfg, reg, logr.Discard())
}

var testClaims = map[string]interface{}{
	"oid":                "abc-123",
	"preferred_username": "alice@contoso.com",
	"tid":                "tenant-456",
	"groups":             []interface{}{"group-a"},
}

func TestCheck_AllowPath(t *testing.T) {
	t.Parallel()
	mock := &mockChecker{decision: &AccessDecision{Allowed: true}}
	srv := newTestServer(t, mock, testConfig())

	req := buildCheckRequest(t, testClaims, "team-alpha.mgr.example.com", "POST", "/v1/chat/completions")
	resp, err := srv.Check(context.Background(), req)
	require.NoError(t, err)

	assert.NotNil(t, resp.GetOkResponse(), "expected OkResponse")
	assert.Equal(t, int32(codes.OK), resp.GetStatus().GetCode())
}

func TestCheck_NoFilterMetadata(t *testing.T) {
	t.Parallel()
	mock := &mockChecker{decision: &AccessDecision{Allowed: true}}
	srv := newTestServer(t, mock, testConfig())

	req := buildCheckRequest(t, nil, "team-alpha.mgr.example.com", "POST", "/v1/chat/completions")
	resp, err := srv.Check(context.Background(), req)
	require.NoError(t, err)

	denied := resp.GetDeniedResponse()
	require.NotNil(t, denied, "expected DeniedResponse")
	assert.Equal(t, typev3.StatusCode_Unauthorized, denied.GetStatus().GetCode())
	assert.Equal(t, 0, mock.calls, "checker should not be called without claims")
}

func TestCheck_CheckerDenies(t *testing.T) {
	t.Parallel()
	mock := &mockChecker{decision: &AccessDecision{Allowed: false, Reason: "not allowed"}}
	srv := newTestServer(t, mock, testConfig())

	req := buildCheckRequest(t, testClaims, "team-alpha.mgr.example.com", "GET", "/v1/models")
	resp, err := srv.Check(context.Background(), req)
	require.NoError(t, err)

	denied := resp.GetDeniedResponse()
	require.NotNil(t, denied, "expected DeniedResponse")
	assert.Equal(t, typev3.StatusCode_Forbidden, denied.GetStatus().GetCode())
	assert.Equal(t, "not allowed", denied.GetBody())
}

func TestCheck_CheckerDeniesDefaultReason(t *testing.T) {
	t.Parallel()
	mock := &mockChecker{decision: &AccessDecision{Allowed: false}}
	srv := newTestServer(t, mock, testConfig())

	req := buildCheckRequest(t, testClaims, "team-alpha.mgr.example.com", "GET", "/v1/models")
	resp, err := srv.Check(context.Background(), req)
	require.NoError(t, err)

	denied := resp.GetDeniedResponse()
	require.NotNil(t, denied, "expected DeniedResponse")
	assert.Equal(t, "access denied", denied.GetBody())
}

func TestCheck_CheckerError_FailClosed(t *testing.T) {
	t.Parallel()
	mock := &mockChecker{err: errors.New("k8s api down")}
	cfg := testConfig()
	cfg.FailOpen = false
	srv := newTestServer(t, mock, cfg)

	req := buildCheckRequest(t, testClaims, "team-alpha.mgr.example.com", "POST", "/v1/chat/completions")
	resp, err := srv.Check(context.Background(), req)
	require.NoError(t, err)

	denied := resp.GetDeniedResponse()
	require.NotNil(t, denied, "expected DeniedResponse")
	assert.Equal(t, typev3.StatusCode_ServiceUnavailable, denied.GetStatus().GetCode())
}

func TestCheck_CheckerError_FailOpen(t *testing.T) {
	t.Parallel()
	mock := &mockChecker{err: errors.New("k8s api down")}
	cfg := testConfig()
	cfg.FailOpen = true
	srv := newTestServer(t, mock, cfg)

	req := buildCheckRequest(t, testClaims, "team-alpha.mgr.example.com", "POST", "/v1/chat/completions")
	resp, err := srv.Check(context.Background(), req)
	require.NoError(t, err)

	assert.NotNil(t, resp.GetOkResponse(), "expected OkResponse (fail-open)")
}

func TestCheck_ForwardedHeaders(t *testing.T) {
	t.Parallel()
	mock := &mockChecker{decision: &AccessDecision{Allowed: true}}
	srv := newTestServer(t, mock, testConfig())

	req := buildCheckRequest(t, testClaims, "team-alpha.mgr.example.com", "POST", "/v1/chat/completions")
	resp, err := srv.Check(context.Background(), req)
	require.NoError(t, err)

	okResp := resp.GetOkResponse()
	require.NotNil(t, okResp)

	headerMap := map[string]string{}
	for _, h := range okResp.GetHeaders() {
		headerMap[h.GetHeader().GetKey()] = h.GetHeader().GetValue()
	}

	assert.Equal(t, "abc-123", headerMap["x-user-oid"])
	assert.Equal(t, "team-alpha", headerMap["x-tenant-namespace"])
	assert.Equal(t, "alice@contoso.com", headerMap["x-auth-username"])
	assert.Equal(t, "tenant-456", headerMap["x-auth-tid"])
	assert.Equal(t, "group-a", headerMap["x-auth-groups"])
}

func TestCheck_NamespaceFromHost(t *testing.T) {
	t.Parallel()
	var capturedReq *AccessRequest
	mock := &mockChecker{decision: &AccessDecision{Allowed: true}}
	wrapper := &capturingChecker{delegate: mock, captured: &capturedReq}
	srv := newTestServer(t, wrapper, testConfig())

	req := buildCheckRequest(t, testClaims, "team-beta.mgr.example.com", "POST", "/v1/chat/completions")
	_, err := srv.Check(context.Background(), req)
	require.NoError(t, err)

	require.NotNil(t, capturedReq)
	assert.Equal(t, "team-beta", capturedReq.ResourceAttributes.Namespace)
	assert.Equal(t, "inference", capturedReq.ResourceAttributes.Verb)
	assert.Equal(t, "aimanager", capturedReq.ResourceAttributes.Group)
	assert.Equal(t, "namespaces", capturedReq.ResourceAttributes.Resource)
}

// capturingChecker wraps an AccessChecker and captures the request.
type capturingChecker struct {
	delegate AccessChecker
	captured **AccessRequest
}

func (c *capturingChecker) Check(ctx context.Context, req *AccessRequest) (*AccessDecision, error) {
	*c.captured = req
	return c.delegate.Check(ctx, req)
}
