package azure

import (
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
)

const (
	// authorizationService is the gRPC service name for health checks.
	authorizationService = "envoy.service.auth.v3.Authorization"
)

// RegisterHealthServer creates and registers a gRPC health server with
// initial SERVING status on the given gRPC server.
func RegisterHealthServer(s *grpc.Server) {
	hs := health.NewServer()
	hs.SetServingStatus("", healthgrpc.HealthCheckResponse_SERVING)
	hs.SetServingStatus(authorizationService, healthgrpc.HealthCheckResponse_SERVING)
	healthgrpc.RegisterHealthServer(s, hs)
}
