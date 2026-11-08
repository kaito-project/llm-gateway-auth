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
	"strings"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"k8s.io/utils/clock"

	"github.com/kaito-project/llm-gateway-auth/internal/authz"
	"github.com/kaito-project/llm-gateway-auth/internal/keyindex"
	"github.com/kaito-project/llm-gateway-auth/internal/keyutil"
)

const (
	authorizationHeader = "Authorization"
	apiKeyHeader        = "API-Key"
	xAPIKeyHeader       = "X-API-Key"
)

var (
	checkDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "apikey_authz_check_duration_seconds",
		Help:    "Check RPC latency",
		Buckets: []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1},
	}, []string{"result"})
)

// Server implements the Envoy ext_authz gRPC Authorization service.
type Server struct {
	authv3.UnimplementedAuthorizationServer
	keyIndex *keyindex.Index
	logger   *zap.Logger
	clock    clock.PassiveClock
}

// NewServer creates a new ext_authz gRPC server.
func NewServer(idx *keyindex.Index, logger *zap.Logger, passiveClock clock.PassiveClock) *Server {
	return &Server{
		keyIndex: idx,
		logger:   logger,
		clock:    passiveClock,
	}
}

func (s *Server) now() time.Time {
	return s.clock.Now()
}

// Check implements envoy.service.auth.v3.Authorization.Check.
func (s *Server) Check(ctx context.Context, req *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	start := time.Now()

	httpReq := req.GetAttributes().GetRequest().GetHttp()
	headers := httpReq.GetHeaders()
	path := httpReq.GetPath()
	sourceIP := req.GetAttributes().GetSource().GetAddress().GetSocketAddress().GetAddress()

	// Resolve gateway namespace:
	//   Priority 1: explicit context_extensions["gateway-namespace"] (backward compat / EnvoyFilter override)
	//   Priority 2: first subdomain of the Host / :authority header (e.g. "team-alpha.gw.example.com" → "team-alpha")
	//   Priority 3: deny — neither source provided a namespace
	gatewayNamespace := req.GetAttributes().GetContextExtensions()["gateway-namespace"]
	if gatewayNamespace == "" {
		gatewayNamespace = authz.NamespaceFromHost(httpReq.GetHost())
	}
	if gatewayNamespace == "" {
		s.recordDeny("deny_missing_gateway", start, "", "", sourceIP, path)
		return denyResponse(codes.InvalidArgument,
			"cannot determine gateway namespace: set context_extensions[gateway-namespace] or use subdomain-based host"), nil
	}

	// 1. Extract the API key in precedence order: Authorization, API-Key, X-API-Key.
	key, credentialHeader := apiKeyFromHeaders(headers)
	if key == "" {
		s.recordDeny("deny_missing_key", start, "", "", sourceIP, path)
		if headerValue(headers, authorizationHeader) != "" {
			return denyResponse(codes.Unauthenticated, "malformed "+authorizationHeader+" header"), nil
		}
		return denyResponse(codes.Unauthenticated, "missing API key credentials: expected "+authorizationHeader+", "+apiKeyHeader+", or "+xAPIKeyHeader+" header"), nil
	}

	// 2. Hash the key
	hash := keyutil.HashKey(key)

	// 3. In-memory index lookup
	rec, ok := s.keyIndex.Lookup(hash)
	if !ok {
		s.recordDeny("deny_invalid_key", start, "", "", sourceIP, path)
		return denyResponse(codes.Unauthenticated, "invalid API key in "+credentialHeader+" header"), nil
	}

	// 4. Enforce expiry immediately after lookup. A zero deadline represents a
	// matched primary status record with no expiry.
	now := s.now()
	if !rec.ExpiresAt.IsZero() && !now.Before(rec.ExpiresAt) {
		s.recordDeny("deny_expired_key", start, rec.KeyID, rec.Namespace, sourceIP, path)
		return denyResponse(codes.Unauthenticated, "invalid API key in "+credentialHeader+" header"), nil
	}

	// 5. Check gateway namespace authorization
	if rec.Namespace != gatewayNamespace {
		s.recordDeny("deny_unauthorized_gateway", start, rec.KeyID, rec.Namespace, sourceIP, path)
		return denyResponse(codes.PermissionDenied, "API key not authorized for this gateway"), nil
	}

	// 6. Allow with tenant headers
	duration := time.Since(start)
	checkDuration.WithLabelValues("allow").Observe(duration.Seconds())

	s.logger.Info("authz.check",
		zap.String("keyID", rec.KeyID),
		zap.String("namespace", rec.Namespace),
		zap.String("result", "allow"),
		zap.String("sourceIP", sourceIP),
		zap.String("path", path),
		zap.Float64("durationMs", float64(duration.Microseconds())/1000.0),
	)

	return allowResponse(rec.Namespace, rec.KeyID), nil
}

func apiKeyFromHeaders(headers map[string]string) (string, string) {
	if authorization := headerValue(headers, authorizationHeader); authorization != "" {
		return extractBearerToken(authorization), authorizationHeader
	}
	if key := headerValue(headers, apiKeyHeader); key != "" {
		return key, apiKeyHeader
	}
	if key := headerValue(headers, xAPIKeyHeader); key != "" {
		return key, xAPIKeyHeader
	}
	return "", ""
}

func headerValue(headers map[string]string, name string) string {
	if value, ok := headers[strings.ToLower(name)]; ok {
		return value
	}
	for headerName, value := range headers {
		if strings.EqualFold(headerName, name) {
			return value
		}
	}
	return ""
}

func extractBearerToken(authHeader string) string {
	if authHeader == "" {
		return ""
	}
	if strings.HasPrefix(authHeader, "Bearer ") {
		return strings.TrimPrefix(authHeader, "Bearer ")
	}
	return ""
}

func (s *Server) recordDeny(result string, start time.Time, keyID, namespace, sourceIP, path string) {
	duration := time.Since(start)
	checkDuration.WithLabelValues(result).Observe(duration.Seconds())

	s.logger.Info("authz.check",
		zap.String("keyID", keyID),
		zap.String("namespace", namespace),
		zap.String("result", result),
		zap.String("sourceIP", sourceIP),
		zap.String("path", path),
		zap.Float64("durationMs", float64(duration.Microseconds())/1000.0),
	)
}

func denyResponse(code codes.Code, msg string) *authv3.CheckResponse {
	return &authv3.CheckResponse{
		Status: &status.Status{
			Code:    int32(code),
			Message: msg,
		},
		HttpResponse: &authv3.CheckResponse_DeniedResponse{
			DeniedResponse: &authv3.DeniedHttpResponse{
				Status: &typev3.HttpStatus{
					Code: typev3.StatusCode_Unauthorized,
				},
				Body: `{"error":"` + msg + `"}`,
				Headers: []*corev3.HeaderValueOption{
					{
						Header: &corev3.HeaderValue{
							Key:   "Content-Type",
							Value: "application/json",
						},
					},
				},
			},
		},
	}
}

func allowResponse(namespace, keyID string) *authv3.CheckResponse {
	return &authv3.CheckResponse{
		Status: &status.Status{
			Code: int32(codes.OK),
		},
		HttpResponse: &authv3.CheckResponse_OkResponse{
			OkResponse: &authv3.OkHttpResponse{
				HeadersToRemove: []string{
					authorizationHeader,
					apiKeyHeader,
					xAPIKeyHeader,
				},
				Headers: []*corev3.HeaderValueOption{
					{
						Header: &corev3.HeaderValue{
							Key:   "x-tenant-namespace",
							Value: namespace,
						},
					},
					{
						Header: &corev3.HeaderValue{
							Key:   "x-key-id",
							Value: keyID,
						},
					},
				},
			},
		},
	}
}
