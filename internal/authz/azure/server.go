package azure

import (
	"context"
	"strings"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	authorizationv1 "k8s.io/api/authorization/v1"

	"github.com/kaito-project/llm-gateway-auth/internal/authz"
)

// AuthorizationServer implements envoy.service.auth.v3.Authorization.
// It reads pre-validated JWT claims from Istio's filter metadata and
// delegates authorization to the Kubernetes API server via SubjectAccessReview.
type AuthorizationServer struct {
	authv3.UnimplementedAuthorizationServer

	checker AccessChecker
	cfg     *Config
	metrics *serverMetrics
	logger  logr.Logger
}

// serverMetrics holds all Prometheus instruments for the authz server.
type serverMetrics struct {
	requestDuration *prometheus.HistogramVec
}

// NewAuthorizationServer creates a new ext_authz gRPC server.
func NewAuthorizationServer(
	checker AccessChecker,
	cfg *Config,
	reg prometheus.Registerer,
	logger logr.Logger,
) *AuthorizationServer {
	m := &serverMetrics{
		requestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "authz_request_duration_seconds",
				Help:    "Duration of authorization check requests.",
				Buckets: []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0},
			},
			[]string{"result"},
		),
	}

	reg.MustRegister(m.requestDuration)

	return &AuthorizationServer{
		checker: checker,
		cfg:     cfg,
		metrics: m,
		logger:  logger.WithName("authz-server"),
	}
}

// Check implements envoy.service.auth.v3.Authorization/Check.
//
// It expects Istio's RequestAuthentication to have already validated the JWT.
// Claims are read from Envoy's filter metadata (envoy.filters.http.jwt_authn).
func (s *AuthorizationServer) Check(ctx context.Context, req *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	start := time.Now()
	httpReq := req.GetAttributes().GetRequest().GetHttp()

	// Step 1: Extract pre-validated claims from Istio's filter metadata.
	filterMetadata := req.GetAttributes().GetMetadataContext().GetFilterMetadata()
	claims, err := claimsFromFilterMetadata(filterMetadata)
	if err != nil {
		s.logger.Info("no valid JWT claims in filter metadata", "error", err, "path", httpReq.GetPath())
		s.metrics.requestDuration.WithLabelValues("deny").Observe(time.Since(start).Seconds())
		return denied(typev3.StatusCode_Unauthorized, "authentication required"), nil
	}

	// Step 2: Map claims to Kubernetes subjects.
	user, groups, extra := toKubernetesSubject(claims)

	// Step 3: Resolve the gateway namespace.
	// Prefer the value supplied by Envoy via ExtAuthzPerRoute context_extensions
	// ("gateway-namespace"): it comes from the data plane's own configuration and,
	// unlike the Host header, cannot be forged by the client. Fall back to the
	// Host-derived value for gateways that predate the per-route config.
	namespaceSource := "context-extension"
	namespace := req.GetAttributes().GetContextExtensions()["gateway-namespace"]
	if namespace == "" {
		namespace = authz.NamespaceFromHost(httpReq.GetHost())
		namespaceSource = "host"
	}
	accessReq := buildAccessRequest(user, groups, extra, namespace, httpReq, s.cfg)

	// Step 4: SubjectAccessReview (with cache).
	decision, err := s.checker.Check(ctx, accessReq)
	if err != nil {
		s.logger.Error(err, "SAR call failed", "user", user)

		if s.cfg.FailOpen {
			s.logger.Info("fail-open: allowing request despite SAR error", "user", user)
			s.metrics.requestDuration.WithLabelValues("allow").Observe(time.Since(start).Seconds())
			return allowed(claims, namespace), nil
		}

		s.metrics.requestDuration.WithLabelValues("error").Observe(time.Since(start).Seconds())
		return denied(typev3.StatusCode_ServiceUnavailable, "authorization service unavailable"), nil
	}

	// Step 5: Build response.
	if decision.Allowed {
		s.logger.V(1).Info("request allowed", "user", user, "path", httpReq.GetPath())
		s.metrics.requestDuration.WithLabelValues("allow").Observe(time.Since(start).Seconds())
		return allowed(claims, namespace), nil
	}

	reason := decision.Reason
	if reason == "" {
		reason = "access denied"
	}
	s.logger.Info("request denied", "user", user, "path", httpReq.GetPath(), "reason", reason, "namespace", namespace, "namespace-source", namespaceSource)
	s.metrics.requestDuration.WithLabelValues("deny").Observe(time.Since(start).Seconds())
	return denied(typev3.StatusCode_Forbidden, reason), nil
}

// buildAccessRequest maps the HTTP request attributes to a Kubernetes
// SubjectAccessReview with ResourceAttributes. The target namespace must be
// resolved by the caller (see Check for the resolution logic — context_extensions
// takes priority over the Host header).
// The verb is taken from configuration (e.g. "inference") rather than
// mapped from the HTTP method.
func buildAccessRequest(
	user string,
	groups []string,
	extra map[string]authorizationv1.ExtraValue,
	namespace string,
	httpReq *authv3.AttributeContext_HttpRequest,
	cfg *Config,
) *AccessRequest {
	return &AccessRequest{
		User:   user,
		Groups: groups,
		Extra:  extra,
		ResourceAttributes: &authorizationv1.ResourceAttributes{
			Namespace: namespace,
			Verb:      cfg.SARVerb,
			Group:     cfg.SARAPIGroup,
			Resource:  cfg.SARResource,
		},
	}
}

// allowed builds an OkHttpResponse with forwarded identity headers.
// The namespace is included as x-tenant-namespace so downstream services
// know the resolved tenant.
func allowed(claims *azureADClaims, namespace string) *authv3.CheckResponse {
	headers := []*corev3.HeaderValueOption{
		{
			Header: &corev3.HeaderValue{Key: "x-user-oid", Value: claims.OID},
		},
	}
	if namespace != "" {
		headers = append(headers, &corev3.HeaderValueOption{
			Header: &corev3.HeaderValue{Key: "x-tenant-namespace", Value: namespace},
		})
	}
	if claims.PreferredUsername != "" {
		headers = append(headers, &corev3.HeaderValueOption{
			Header: &corev3.HeaderValue{Key: "x-auth-username", Value: claims.PreferredUsername},
		})
	}
	if len(claims.Groups) > 0 {
		headers = append(headers, &corev3.HeaderValueOption{
			Header: &corev3.HeaderValue{Key: "x-auth-groups", Value: strings.Join(claims.Groups, ",")},
		})
	}
	if claims.TenantID != "" {
		headers = append(headers, &corev3.HeaderValueOption{
			Header: &corev3.HeaderValue{Key: "x-auth-tid", Value: claims.TenantID},
		})
	}

	return &authv3.CheckResponse{
		Status: &status.Status{Code: int32(codes.OK)},
		HttpResponse: &authv3.CheckResponse_OkResponse{
			OkResponse: &authv3.OkHttpResponse{
				Headers: headers,
			},
		},
	}
}

// denied builds a DeniedHttpResponse with the given HTTP status and message.
func denied(httpStatus typev3.StatusCode, message string) *authv3.CheckResponse {
	return &authv3.CheckResponse{
		Status: &status.Status{Code: int32(codes.PermissionDenied)},
		HttpResponse: &authv3.CheckResponse_DeniedResponse{
			DeniedResponse: &authv3.DeniedHttpResponse{
				Status: &typev3.HttpStatus{Code: httpStatus},
				Body:   message,
			},
		},
	}
}
