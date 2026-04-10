//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/kaito-project/llm-gateway-auth/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

// Kubernetes coordinates for the authz service (Helm chart).
const (
	authzNamespace   = "llm-gateway"
	authzServiceName = "llm-gateway-apikey-authz"
	authzGRPCPort    = 9001
)

// Well-known names the operator uses inside every tenant namespace.
const (
	apiKeyName    = "default"     // enforced by webhook
	apiKeySecret  = "llm-api-key" // Secret written by operator
	defaultPrefix = "sk-"
)

// Gateway domain suffix used to build per-namespace host headers.
const gwDomain = "gw.example.com"

// Polling / timeout knobs.
const (
	defaultTimeout  = 30 * time.Second
	indexSyncWait   = 5 * time.Second
	pollInterval    = 500 * time.Millisecond
	portFwdTimeout  = 15 * time.Second
	portFwdDialWait = 250 * time.Millisecond
)

// ---------------------------------------------------------------------------
// Namespace helper
// ---------------------------------------------------------------------------

// createNamespace creates a uniquely-named namespace, registers cleanup,
// and returns the name. prefix appears in the name for log readability.
func createNamespace(t *testing.T, prefix string) string {
	t.Helper()
	name := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	require.NoError(t, k8sClient.Create(ctx, ns))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), ns)
	})
	return name
}

// ---------------------------------------------------------------------------
// APIKey helper
// ---------------------------------------------------------------------------

// createAPIKey creates the canonical "default" APIKey in namespace, registers
// cleanup, and returns the object so callers can mutate it later.
func createAPIKey(t *testing.T, namespace, expiresIn string) *v1alpha1.APIKey {
	t.Helper()
	key := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{Name: apiKeyName, Namespace: namespace},
		Spec: v1alpha1.APIKeySpec{
			Prefix:    defaultPrefix,
			ExpiresIn: expiresIn,
		},
	}
	require.NoError(t, k8sClient.Create(ctx, key))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), key) })
	return key
}

// ---------------------------------------------------------------------------
// Authz gRPC client
// ---------------------------------------------------------------------------

// setupAuthzClient port-forwards to the authz service, opens a gRPC
// connection, and returns a ready client. Both are torn down on t.Cleanup.
func setupAuthzClient(t *testing.T) authv3.AuthorizationClient {
	t.Helper()
	addr := portForwardAuthz(t)
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return authv3.NewAuthorizationClient(conn)
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
	deadline := time.Now().Add(portFwdTimeout)
	for time.Now().Before(deadline) {
		conn, dialErr := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			return addr
		}
		time.Sleep(portFwdDialWait)
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

// ---------------------------------------------------------------------------
// Polling helpers
// ---------------------------------------------------------------------------

// waitForAPIKey polls until predicate returns true, or until timeout.
// condition is a human-readable description used in the timeout message.
func waitForAPIKey(
	t *testing.T,
	namespace, name string,
	timeout time.Duration,
	predicate func(*v1alpha1.APIKey) bool,
	condition string,
) *v1alpha1.APIKey {
	t.Helper()
	deadline := time.Now().Add(timeout)
	key := &v1alpha1.APIKey{}
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, key); err == nil {
			if predicate(key) {
				return key
			}
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("timed out after %s waiting for APIKey %s/%s: %s (current phase: %s)",
		timeout, namespace, name, condition, key.Status.Phase)
	return nil
}

// waitForAPIKeyPhase polls until the APIKey reaches the expected phase.
func waitForAPIKeyPhase(t *testing.T, namespace, name string, phase v1alpha1.APIKeyPhase, timeout time.Duration) *v1alpha1.APIKey {
	t.Helper()
	return waitForAPIKey(t, namespace, name, timeout,
		func(k *v1alpha1.APIKey) bool { return k.Status.Phase == phase },
		fmt.Sprintf("phase == %s", phase),
	)
}

// waitForAPIKeyRotation polls until the APIKey has status.Previous set (rotation completed).
func waitForAPIKeyRotation(t *testing.T, namespace, name string, timeout time.Duration) *v1alpha1.APIKey {
	t.Helper()
	return waitForAPIKey(t, namespace, name, timeout,
		func(k *v1alpha1.APIKey) bool {
			return k.Status.Previous != nil && k.Status.LastRotatedAt != nil
		},
		"rotation completed (status.Previous set)",
	)
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
		time.Sleep(pollInterval)
	}
	t.Fatalf("timed out waiting for Secret %s/%s", namespace, secretName)
	return nil
}

// ---------------------------------------------------------------------------
// Condition helper
// ---------------------------------------------------------------------------

// findCondition returns the first Condition with the given type, or nil.
func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Assertion helpers
// ---------------------------------------------------------------------------

// assertAllowed asserts code == 0 and returns the response for header inspection.
func assertAllowed(t *testing.T, cl authv3.AuthorizationClient, req *authv3.CheckRequest) *authv3.CheckResponse {
	t.Helper()
	resp, err := cl.Check(ctx, req)
	require.NoError(t, err)
	assert.EqualValues(t, 0, resp.GetStatus().GetCode(),
		"expected ALLOW (code 0), got code=%v msg=%q",
		resp.GetStatus().GetCode(), resp.GetStatus().GetMessage())
	return resp
}

// assertDenied asserts code != 0 and logs the denial for diagnosis.
func assertDenied(t *testing.T, cl authv3.AuthorizationClient, req *authv3.CheckRequest) {
	t.Helper()
	resp, err := cl.Check(ctx, req)
	require.NoError(t, err)
	assert.NotEqualValues(t, 0, resp.GetStatus().GetCode(),
		"expected DENY, got ALLOW (code=0)")
	t.Logf("denied: code=%d msg=%s", resp.GetStatus().GetCode(), resp.GetStatus().GetMessage())
}

// namespaceHost builds the gateway host header for a tenant namespace.
func namespaceHost(ns string) string {
	return fmt.Sprintf("%s.%s", ns, gwDomain)
}
