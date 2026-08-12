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

package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	crzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	apikeyauthz "github.com/kaito-project/llm-gateway-auth/internal/authz/apikey"
	"github.com/kaito-project/llm-gateway-auth/internal/controller"
	"github.com/kaito-project/llm-gateway-auth/internal/keyindex"
)

// grpcRunnable wraps a gRPC server as a controller-runtime Runnable
// so the manager handles its lifecycle alongside controllers.
type grpcRunnable struct {
	server *grpc.Server
	addr   string
}

func (r *grpcRunnable) Start(ctx context.Context) error {
	lis, err := net.Listen("tcp", r.addr)
	if err != nil {
		return fmt.Errorf("gRPC listen %s: %w", r.addr, err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- r.server.Serve(lis) }()
	select {
	case <-ctx.Done():
		r.server.GracefulStop()
		return nil
	case err := <-errCh:
		return fmt.Errorf("gRPC server error: %w", err)
	}
}

func main() {
	var metricsAddr string
	var probeAddr string
	var grpcAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8082", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8083", "The address the health probe endpoint binds to.")
	flag.StringVar(&grpcAddr, "grpc-bind-address", ":9001", "The address the gRPC ext_authz endpoint binds to.")
	opts := crzap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(crzap.New(crzap.UseFlagOptions(&opts)))

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		ctrl.Log.Error(err, "unable to add clientgo scheme")
		os.Exit(1)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		ctrl.Log.Error(err, "unable to add corev1 scheme")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		ctrl.Log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Shared in-memory key index: IndexSyncer writes, gRPC server reads.
	// keyindex.Index is protected by sync.RWMutex — safe for concurrent use.
	idx := keyindex.New()

	if err := (&controller.IndexSyncer{
		Client:   mgr.GetClient(),
		KeyIndex: idx,
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to create IndexSyncer")
		os.Exit(1)
	}

	// Wire up the API key ext_authz gRPC server.
	zapLog, err := zap.NewProduction()
	if err != nil {
		ctrl.Log.Error(err, "unable to create zap logger")
		os.Exit(1)
	}
	defer zapLog.Sync() //nolint:errcheck

	authzSrv := apikeyauthz.NewServer(idx, zapLog)
	grpcServer := grpc.NewServer()
	authv3.RegisterAuthorizationServer(grpcServer, authzSrv)

	// Register gRPC health service for Kubernetes gRPC probes.
	healthSrv := health.NewServer()
	healthpb.RegisterHealthServer(grpcServer, healthSrv)
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)

	if err := mgr.Add(&grpcRunnable{server: grpcServer, addr: grpcAddr}); err != nil {
		ctrl.Log.Error(err, "unable to add gRPC runnable")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ctrl.Log.Info("starting authz-plugin", "grpcAddr", grpcAddr)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "problem running authz-plugin")
		os.Exit(1)
	}
}
