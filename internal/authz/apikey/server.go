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

	"github.com/kaito-project/llm-gateway-auth/internal/keyindex"
	"github.com/kaito-project/llm-gateway-auth/internal/keyutil"
)

var (
	checkTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_authz_check_total",
		Help: "Auth check result distribution",
	}, []string{"result"})

	checkDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "llm_authz_check_duration_seconds",
		Help:    "Check RPC latency",
		Buckets: []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1},
	}, []string{"result"})
)

// Server implements the Envoy ext_authz gRPC Authorization service.
type Server struct {
	authv3.UnimplementedAuthorizationServer
	keyIndex *keyindex.Index
	logger   *zap.Logger
}

// NewServer creates a new ext_authz gRPC server.
func NewServer(idx *keyindex.Index, logger *zap.Logger) *Server {
	return &Server{
		keyIndex: idx,
		logger:   logger,
	}
}

// Check implements envoy.service.auth.v3.Authorization.Check.
func (s *Server) Check(ctx context.Context, req *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	start := time.Now()

	httpReq := req.GetAttributes().GetRequest().GetHttp()
	headers := httpReq.GetHeaders()
	path := httpReq.GetPath()
	sourceIP := req.GetAttributes().GetSource().GetAddress().GetSocketAddress().GetAddress()

	// Extract gateway namespace from Envoy context_extensions (required)
	gatewayNamespace := req.GetAttributes().GetContextExtensions()["gateway-namespace"]
	if gatewayNamespace == "" {
		s.recordDeny("deny_missing_gateway", start, "", "", sourceIP, path)
		return denyResponse(codes.InvalidArgument, "missing gateway-namespace in context_extensions"), nil
	}

	// 1. Extract Authorization header
	authHeader := headers["authorization"]
	rawKey := extractKey(authHeader)
	if rawKey == "" {
		s.recordDeny("deny_missing_key", start, "", "", sourceIP, path)
		return denyResponse(codes.Unauthenticated, "missing or malformed Authorization header"), nil
	}

	// 2. Format validation
	if !keyutil.ValidateKeyFormat(rawKey) {
		s.recordDeny("deny_invalid_key", start, "", "", sourceIP, path)
		return denyResponse(codes.Unauthenticated, "invalid API key format"), nil
	}

	// 3. Hash the key
	hash := keyutil.HashKey(rawKey)

	// 4. In-memory index lookup
	rec, ok := s.keyIndex.Lookup(hash)
	if !ok {
		s.recordDeny("deny_invalid_key", start, "", "", sourceIP, path)
		return denyResponse(codes.Unauthenticated, "invalid API key"), nil
	}

	// 5. Check gateway namespace authorization
	if rec.Namespace != gatewayNamespace {
		s.recordDeny("deny_unauthorized_gateway", start, rec.KeyID, rec.Namespace, sourceIP, path)
		return denyResponse(codes.PermissionDenied, "API key not authorized for this gateway"), nil
	}

	// 6. Allow with tenant headers
	duration := time.Since(start)
	checkTotal.WithLabelValues("allow").Inc()
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

func extractKey(authHeader string) string {
	if authHeader == "" {
		return ""
	}
	if strings.HasPrefix(authHeader, "Bearer ") {
		return strings.TrimPrefix(authHeader, "Bearer ")
	}
	if strings.HasPrefix(authHeader, "sk-") {
		return authHeader
	}
	return ""
}

func (s *Server) recordDeny(result string, start time.Time, keyID, namespace, sourceIP, path string) {
	duration := time.Since(start)
	checkTotal.WithLabelValues(result).Inc()
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
