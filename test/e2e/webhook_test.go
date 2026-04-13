//go:build e2e

// Package e2e contains end-to-end tests for the APIKey validating webhook.
//
// These tests run against a real Kubernetes cluster (kind) with
// USE_EXISTING_CLUSTER=true so envtest connects to it instead of
// starting its own control plane.
//
// Prerequisites:
//
//	make kind-create          # creates the kind cluster (SKIP_ISTIO=true)
//
// Run with:
//
//	make e2e-test
package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha1 "github.com/kaito-project/llm-gateway-auth/api/v1alpha1"
	apikeywebhook "github.com/kaito-project/llm-gateway-auth/internal/webhook"
)

var (
	k8sClient client.Client
	testEnv   *envtest.Environment
	ctx       context.Context
	cancel    context.CancelFunc
	scheme    = runtime.NewScheme()
)

// kindHostIP returns an IP reachable from inside the kind control-plane container
// back to the host machine. On Podman (macOS), containers can reach the host
// via host.containers.internal. On Docker, the container gateway works.
func kindHostIP(clusterName string) string {
	containerName := clusterName + "-control-plane"
	rt := "docker"
	if _, err := exec.LookPath("podman"); err == nil {
		rt = "podman"
	}

	// Try resolving host.containers.internal from inside the container (Podman).
	out, err := exec.Command(rt, "exec", containerName,
		"getent", "hosts", "host.containers.internal").Output()
	if err == nil {
		fields := strings.Fields(strings.TrimSpace(string(out)))
		if len(fields) > 0 {
			return fields[0]
		}
	}

	// Fall back to the container gateway IP (Docker).
	out, err = exec.Command(rt, "inspect", containerName,
		"--format", "{{range .NetworkSettings.Networks}}{{.Gateway}}{{end}}").Output()
	if err == nil && strings.TrimSpace(string(out)) != "" {
		return strings.TrimSpace(string(out))
	}

	return ""
}

func TestMain(m *testing.M) {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	_ = clientgoscheme.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)

	webhookOpts := envtest.WebhookInstallOptions{
		Paths: []string{"../../config/webhook"},
	}

	// When using an existing cluster (kind), the API server runs inside a
	// container and cannot reach 127.0.0.1 on the host. We detect the
	// container gateway IP and tell envtest to advertise that address in
	// the webhook URL while still listening on 0.0.0.0 locally.
	clusterName := os.Getenv("KIND_CLUSTER_NAME")
	if clusterName == "" {
		clusterName = "llm-gateway-test"
	}
	if gateway := kindHostIP(clusterName); gateway != "" {
		webhookOpts.LocalServingHost = "0.0.0.0"
		webhookOpts.LocalServingHostExternalName = gateway
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../config/crd/bases"},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: webhookOpts,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start test environment: %v\n", err)
		os.Exit(1)
	}

	// Start a manager that serves the webhook.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    testEnv.WebhookInstallOptions.LocalServingHost,
			Port:    testEnv.WebhookInstallOptions.LocalServingPort,
			CertDir: testEnv.WebhookInstallOptions.LocalServingCertDir,
		}),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create manager: %v\n", err)
		os.Exit(1)
	}

	mgr.GetWebhookServer().Register(
		"/validate-kaito-sh-v1alpha1-apikey",
		admission.WithValidator[*v1alpha1.APIKey](scheme, &apikeywebhook.APIKeyValidator{}),
	)

	ctx, cancel = context.WithCancel(context.Background())
	go func() {
		if err := mgr.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "manager exited: %v\n", err)
		}
	}()

	// Wait for the webhook TLS listener to be ready. mgr.Elected() only
	// signals that leader-election runnables have started — the webhook
	// server may still be binding its port. Poll with a real TLS dial so
	// we know the listener is accepting connections before running tests.
	webhookAddr := fmt.Sprintf("127.0.0.1:%d", testEnv.WebhookInstallOptions.LocalServingPort)
	tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test-only self-signed cert
	deadline := time.Now().Add(30 * time.Second)
	for {
		conn, dialErr := tls.Dial("tcp", webhookAddr, tlsCfg)
		if dialErr == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "webhook server never became ready at %s: %v\n", webhookAddr, dialErr)
			os.Exit(1)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Single non-cached client — all reads and writes go directly to the API server.
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create client: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	// envtest does not delete the ValidatingWebhookConfiguration it installed
	// when USE_EXISTING_CLUSTER=true. Remove it explicitly so the operator
	// (still running in the cluster) can reconcile APIKey resources after
	// the test binary exits.
	vwc := &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "validating-webhook-configuration"},
	}
	_ = k8sClient.Delete(context.Background(), vwc)

	cancel()
	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to stop test environment: %v\n", err)
	}
	os.Exit(code)
}

func TestCreateValidAPIKey(t *testing.T) {
	ns := createNamespace(t, "e2e-webhook")
	key := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns},
		Spec: v1alpha1.APIKeySpec{
			Prefix:    "sk-",
			ExpiresIn: "720h",
		},
	}
	require.NoError(t, k8sClient.Create(ctx, key))
}

func TestCreateInvalidName(t *testing.T) {
	ns := createNamespace(t, "e2e-webhook")
	key := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{Name: "not-default", Namespace: ns},
		Spec:       v1alpha1.APIKeySpec{Prefix: "sk-"},
	}
	err := k8sClient.Create(ctx, key)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be \"default\"")
}

func TestCreateInvalidExpiresIn(t *testing.T) {
	ns := createNamespace(t, "e2e-webhook")
	key := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns},
		Spec:       v1alpha1.APIKeySpec{Prefix: "sk-", ExpiresIn: "abc"},
	}
	err := k8sClient.Create(ctx, key)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be a valid Go duration")
}

func TestCreateInvalidGracePeriod(t *testing.T) {
	ns := createNamespace(t, "e2e-webhook")
	key := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns},
		Spec: v1alpha1.APIKeySpec{
			Prefix: "sk-",
			Rotation: &v1alpha1.RotationSpec{
				GracePeriod: "invalid",
			},
		},
	}
	err := k8sClient.Create(ctx, key)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be a valid Go duration")
}

func TestCreateRotateAtInPast(t *testing.T) {
	ns := createNamespace(t, "e2e-webhook")
	past := metav1.NewTime(time.Now().Add(-1 * time.Hour))
	key := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns},
		Spec: v1alpha1.APIKeySpec{
			Prefix:   "sk-",
			Rotation: &v1alpha1.RotationSpec{RotateAt: &past},
		},
	}
	err := k8sClient.Create(ctx, key)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be a time in the future")
}

func TestUpdatePrefixImmutable(t *testing.T) {
	ns := createNamespace(t, "e2e-webhook")
	key := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns},
		Spec:       v1alpha1.APIKeySpec{Prefix: "sk-"},
	}
	require.NoError(t, k8sClient.Create(ctx, key))

	// RetryOnConflict handles the case where the operator modifies the resource
	// (adding finalizers, updating status) between our Get and Update calls.
	// It transparently re-fetches and retries on conflict, surfacing only
	// non-conflict errors (e.g. the webhook rejection we expect here).
	updateErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(key), key); err != nil {
			return err
		}
		key.Spec.Prefix = "changed-"
		return k8sClient.Update(ctx, key)
	})
	require.Error(t, updateErr)
	assert.Contains(t, updateErr.Error(), "immutable")
}

func TestUpdateExpiresInWithoutRotateAtRejected(t *testing.T) {
	ns := createNamespace(t, "e2e-webhook")
	key := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns},
		Spec:       v1alpha1.APIKeySpec{Prefix: "sk-", ExpiresIn: "720h"},
	}
	require.NoError(t, k8sClient.Create(ctx, key))

	updateErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(key), key); err != nil {
			return err
		}
		key.Spec.ExpiresIn = "48h"
		return k8sClient.Update(ctx, key)
	})
	require.Error(t, updateErr)
	assert.Contains(t, updateErr.Error(), "immutable")
}

func TestUpdateExpiresInWithRotateAtAllowed(t *testing.T) {
	ns := createNamespace(t, "e2e-webhook")
	key := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns},
		Spec:       v1alpha1.APIKeySpec{Prefix: "sk-", ExpiresIn: "720h"},
	}
	require.NoError(t, k8sClient.Create(ctx, key))

	updateErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(key), key); err != nil {
			return err
		}
		future := metav1.NewTime(time.Now().Add(24 * time.Hour))
		key.Spec.ExpiresIn = "48h"
		key.Spec.Rotation = &v1alpha1.RotationSpec{RotateAt: &future}
		return k8sClient.Update(ctx, key)
	})
	require.NoError(t, updateErr)
}

func TestDeleteAlwaysAllowed(t *testing.T) {
	ns := createNamespace(t, "e2e-webhook")
	key := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns},
		Spec:       v1alpha1.APIKeySpec{Prefix: "sk-"},
	}
	require.NoError(t, k8sClient.Create(ctx, key))
	require.NoError(t, k8sClient.Delete(ctx, key))
}
