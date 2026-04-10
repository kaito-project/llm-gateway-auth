//go:build e2e

// Package e2e contains end-to-end tests that run against a real kind cluster.
//
// This file tests the full API key lifecycle:
//   - Create APIKey CR → operator creates Secret with the raw key
//   - IndexSyncer populates the in-memory index from the Secret
//   - gRPC ext_authz Check: allow with correct key+host, deny otherwise
//
// Prerequisites:
//
//	make setup-test-env                          # kind + Istio (or kind-create for no Istio)
//	make kind-load-operator kind-load-authz      # build + load images
//	helm install llm-gateway-apikey chart/llm-gateway-apikey \
//	  --namespace llm-gateway --create-namespace \
//	  --set operator.image.repository=localhost/apikey-operator \
//	  --set operator.image.tag=latest \
//	  --set authz.image.repository=localhost/apikey-istio-authz-ext \
//	  --set authz.image.tag=latest \
//	  --set global.imagePullPolicy=Never \
//	  --set istio.enabled=false \
//	  --set operator.webhook.enabled=false \
//	  --set crds.install=false
//
// Run with:
//
//	make e2e-test
package e2e

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/kaito-project/llm-gateway-auth/api/v1alpha1"
)

// authzNamespace is where the Helm chart deploys the authz service.
const authzNamespace = "llm-gateway"

// authzServiceName is the Kubernetes Service for the gRPC ext_authz.
const authzServiceName = "llm-gateway-apikey-authz"

// authzGRPCPort is the gRPC port exposed by the authz Service.
const authzGRPCPort = 9001

// createAPIKeyNamespace creates a namespace for the test and returns its name.
func createAPIKeyNamespace(t *testing.T, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	err := k8sClient.Create(ctx, ns)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), ns)
	})
}

// waitForSecret polls until the named Secret appears in the given namespace.
func waitForSecret(t *testing.T, namespace, secretName string, timeout time.Duration) *corev1.Secret {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sec := &corev1.Secret{}
		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: secretName}, sec)
		if err == nil {
			return sec
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for Secret %s/%s", namespace, secretName)
	return nil
}

// portForwardAuthz starts kubectl port-forward to the authz Service and returns
// the local address (host:port). The port-forward is terminated on test cleanup.
func portForwardAuthz(t *testing.T) string {
	t.Helper()

	// Find a free local port.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	localPort := lis.Addr().(*net.TCPAddr).Port
	lis.Close()

	cmd := exec.CommandContext(ctx,
		"kubectl", "--context", "kind-llm-gateway-test",
		"port-forward", "-n", authzNamespace,
		fmt.Sprintf("svc/%s", authzServiceName),
		fmt.Sprintf("%d:%d", localPort, authzGRPCPort),
	)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	// Wait for port-forward to be ready.
	addr := fmt.Sprintf("127.0.0.1:%d", localPort)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return addr
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for port-forward to %s", addr)
	return ""
}

// makeCheckRequest builds an ext_authz CheckRequest with the given host and auth header.
func makeCheckRequest(host, authHeader, path string) *authv3.CheckRequest {
	headers := map[string]string{}
	if authHeader != "" {
		headers["authorization"] = authHeader
	}
	return &authv3.CheckRequest{
		Attributes: &authv3.AttributeContext{
			Request: &authv3.AttributeContext_Request{
				Http: &authv3.AttributeContext_HttpRequest{
					Host:    host,
					Path:    path,
					Headers: headers,
				},
			},
			Source: &authv3.AttributeContext_Peer{
				Address: &corev3.Address{
					Address: &corev3.Address_SocketAddress{
						SocketAddress: &corev3.SocketAddress{
							Address: "10.0.0.1",
						},
					},
				},
			},
		},
	}
}

func TestAPIKeyLifecycle(t *testing.T) {
	testNS := fmt.Sprintf("e2e-apikey-%d", time.Now().UnixNano())

	// 1. Create test namespace.
	createAPIKeyNamespace(t, testNS)

	// 2. Create an APIKey CR.
	apiKey := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default",
			Namespace: testNS,
		},
		Spec: v1alpha1.APIKeySpec{
			Prefix:    "sk-",
			ExpiresIn: "720h",
		},
	}
	require.NoError(t, k8sClient.Create(ctx, apiKey))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), apiKey)
	})

	// 3. Wait for the operator to create the Secret.
	sec := waitForSecret(t, testNS, "llm-api-key", 30*time.Second)
	rawKey := string(sec.Data["apiKey"])
	require.NotEmpty(t, rawKey, "Secret should contain an apiKey")
	require.True(t, strings.HasPrefix(rawKey, "sk-"), "key should have the configured prefix")
	t.Logf("API key created: %s... (len=%d)", rawKey[:6], len(rawKey))

	// 4. Port-forward to the authz gRPC service.
	addr := portForwardAuthz(t)

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	authzClient := authv3.NewAuthorizationClient(conn)

	// Give the IndexSyncer time to pick up the new Secret.
	// The syncer watches Secrets; it should reconcile within a few seconds.
	time.Sleep(5 * time.Second)

	// 5. Check: valid key + correct namespace host → ALLOW.
	t.Run("allow_valid_key_correct_host", func(t *testing.T) {
		host := fmt.Sprintf("%s.gw.example.com", testNS)
		req := makeCheckRequest(host, "Bearer "+rawKey, "/v1/chat/completions")
		resp, err := authzClient.Check(ctx, req)
		require.NoError(t, err)
		require.EqualValues(t, 0, resp.GetStatus().GetCode(), "expected OK (code 0), got: %v — %s", resp.GetStatus().GetCode(), resp.GetStatus().GetMessage())

		// Verify tenant headers are injected.
		okResp := resp.GetOkResponse()
		require.NotNil(t, okResp, "expected OkResponse")
		headerMap := map[string]string{}
		for _, h := range okResp.GetHeaders() {
			headerMap[h.GetHeader().GetKey()] = h.GetHeader().GetValue()
		}
		require.Equal(t, testNS, headerMap["x-tenant-namespace"])
		require.NotEmpty(t, headerMap["x-key-id"])
		t.Logf("x-tenant-namespace=%s x-key-id=%s", headerMap["x-tenant-namespace"], headerMap["x-key-id"])
	})

	// 6. Check: valid key + wrong namespace host → DENY.
	t.Run("deny_valid_key_wrong_host", func(t *testing.T) {
		req := makeCheckRequest("wrong-namespace.gw.example.com", "Bearer "+rawKey, "/v1/chat/completions")
		resp, err := authzClient.Check(ctx, req)
		require.NoError(t, err)
		require.NotEqualValues(t, 0, resp.GetStatus().GetCode(), "expected deny, got allow")
		require.NotNil(t, resp.GetDeniedResponse(), "expected DeniedResponse")
		t.Logf("denied: code=%d msg=%s", resp.GetStatus().GetCode(), resp.GetStatus().GetMessage())
	})

	// 7. Check: missing Authorization header → DENY.
	t.Run("deny_missing_auth_header", func(t *testing.T) {
		host := fmt.Sprintf("%s.gw.example.com", testNS)
		req := makeCheckRequest(host, "", "/v1/chat/completions")
		resp, err := authzClient.Check(ctx, req)
		require.NoError(t, err)
		require.NotEqualValues(t, 0, resp.GetStatus().GetCode(), "expected deny, got allow")
		t.Logf("denied: code=%d msg=%s", resp.GetStatus().GetCode(), resp.GetStatus().GetMessage())
	})

	// 8. Check: invalid key → DENY.
	t.Run("deny_invalid_key", func(t *testing.T) {
		host := fmt.Sprintf("%s.gw.example.com", testNS)
		req := makeCheckRequest(host, "Bearer sk-totally-bogus-key", "/v1/chat/completions")
		resp, err := authzClient.Check(ctx, req)
		require.NoError(t, err)
		require.NotEqualValues(t, 0, resp.GetStatus().GetCode(), "expected deny, got allow")
		t.Logf("denied: code=%d msg=%s", resp.GetStatus().GetCode(), resp.GetStatus().GetMessage())
	})

	// 9. Check: no host subdomain → DENY (cannot resolve namespace).
	t.Run("deny_no_host_subdomain", func(t *testing.T) {
		req := makeCheckRequest("localhost", "Bearer "+rawKey, "/v1/chat/completions")
		resp, err := authzClient.Check(ctx, req)
		require.NoError(t, err)
		require.NotEqualValues(t, 0, resp.GetStatus().GetCode(), "expected deny, got allow")
		t.Logf("denied: code=%d msg=%s", resp.GetStatus().GetCode(), resp.GetStatus().GetMessage())
	})
}
