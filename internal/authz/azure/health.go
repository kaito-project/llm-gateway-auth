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
