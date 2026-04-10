// Package main is the entrypoint for the ext_authz gRPC server.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"github.com/go-logr/zapr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	azure "github.com/kaito-project/llm-gateway-auth/internal/authz/azure"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Parse configuration.
	cfg, err := azure.ParseFlags()
	if err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}

	// Set up structured logging.
	zapLog, err := zap.NewProduction()
	if err != nil {
		return fmt.Errorf("creating logger: %w", err)
	}
	defer zapLog.Sync() //nolint:errcheck
	logger := zapr.NewLogger(zapLog)

	// Set up context with signal handling.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Build Kubernetes client.
	k8sClient, err := buildKubernetesClient(cfg.KubeconfigPath)
	if err != nil {
		return fmt.Errorf("building kubernetes client: %w", err)
	}

	// Set up Prometheus registry.
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	reg.MustRegister(collectors.NewGoCollector())

	// Initialize SAR checker with LRU cache.
	sarChecker := azure.NewKubernetesAccessChecker(k8sClient, logger)
	checker, err := azure.WithCache(sarChecker, cfg.CacheSize, cfg.CacheTTL, reg, logger)
	if err != nil {
		return fmt.Errorf("creating cached checker: %w", err)
	}

	// Create the authz server.
	authzServer := azure.NewAuthorizationServer(checker, cfg, reg, logger)

	// Build gRPC server options.
	var grpcOpts []grpc.ServerOption
	if cfg.TLSCertFile != "" {
		tlsCert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return fmt.Errorf("loading TLS certificate: %w", err)
		}
		grpcOpts = append(grpcOpts, grpc.Creds(credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{tlsCert},
			MinVersion:   tls.VersionTLS12,
		})))
	}

	grpcServer := grpc.NewServer(grpcOpts...)
	authv3.RegisterAuthorizationServer(grpcServer, authzServer)

	// Register gRPC health service.
	azure.RegisterHealthServer(grpcServer)

	// Start HTTP metrics server.
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	metricsServer := &http.Server{Addr: cfg.MetricsAddr, Handler: metricsMux}
	go func() {
		logger.Info("metrics server starting", "addr", cfg.MetricsAddr)
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error(err, "metrics server failed")
		}
	}()

	// Start gRPC listener.
	lis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.GRPCAddr, err)
	}
	logger.Info("gRPC server starting", "addr", cfg.GRPCAddr, "fail-open", cfg.FailOpen)

	// Run gRPC server in a goroutine so we can wait for shutdown signal.
	errCh := make(chan error, 1)
	go func() {
		errCh <- grpcServer.Serve(lis)
	}()

	// Wait for shutdown signal or server error.
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining...")
	case err := <-errCh:
		return fmt.Errorf("gRPC server error: %w", err)
	}

	// Graceful shutdown.
	grpcServer.GracefulStop()
	if err := metricsServer.Close(); err != nil {
		logger.Error(err, "error closing metrics server")
	}
	logger.Info("server stopped")
	return nil
}

// buildKubernetesClient creates a Kubernetes clientset using in-cluster config
// or the provided kubeconfig path for local development.
func buildKubernetesClient(kubeconfigPath string) (kubernetes.Interface, error) {
	var restConfig *rest.Config
	var err error

	if kubeconfigPath != "" {
		restConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	} else {
		restConfig, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("building REST config: %w", err)
	}

	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes client: %w", err)
	}

	return client, nil
}
