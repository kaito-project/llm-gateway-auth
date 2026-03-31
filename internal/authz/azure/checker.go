package azure

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/prometheus/client_golang/prometheus"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// AccessRequest is the normalized input for an authorization check.
type AccessRequest struct {
	// User is the Kubernetes subject (mapped from Azure AD OID).
	User string
	// Groups are the Kubernetes groups (mapped from Azure AD group OIDs).
	Groups []string
	// Extra carries additional claim metadata for audit purposes.
	Extra map[string]authorizationv1.ExtraValue
	// ResourceAttributes is set for resource-scoped requests.
	ResourceAttributes *authorizationv1.ResourceAttributes
	// NonResourceAttributes is set for non-resource requests (e.g. /healthz).
	NonResourceAttributes *authorizationv1.NonResourceAttributes
}

// AccessDecision is the normalized output of an authorization check.
type AccessDecision struct {
	// Allowed indicates whether the request is authorized.
	Allowed bool
	// Denied indicates an explicit deny (as opposed to an implicit no-opinion).
	Denied bool
	// Reason is the human-readable reason from the API server.
	Reason string
	// EvaluationError is set when the API server encountered an error evaluating the request.
	EvaluationError string
}

// AccessChecker abstracts the Kubernetes SubjectAccessReview call
// to allow test mocking.
type AccessChecker interface {
	Check(ctx context.Context, req *AccessRequest) (*AccessDecision, error)
}

// KubernetesAccessChecker performs SubjectAccessReviews against the
// Kubernetes API server.
type KubernetesAccessChecker struct {
	client kubernetes.Interface
	logger logr.Logger
}

// NewKubernetesAccessChecker creates a checker backed by the given Kubernetes client.
func NewKubernetesAccessChecker(client kubernetes.Interface, logger logr.Logger) *KubernetesAccessChecker {
	return &KubernetesAccessChecker{
		client: client,
		logger: logger.WithName("sar-checker"),
	}
}

// Check submits a SubjectAccessReview to the Kubernetes API server.
func (k *KubernetesAccessChecker) Check(ctx context.Context, req *AccessRequest) (*AccessDecision, error) {
	sar := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:                  req.User,
			Groups:                req.Groups,
			Extra:                 req.Extra,
			ResourceAttributes:    req.ResourceAttributes,
			NonResourceAttributes: req.NonResourceAttributes,
		},
	}

	result, err := k.client.AuthorizationV1().SubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("SubjectAccessReview API call failed: %w", err)
	}

	decision := &AccessDecision{
		Allowed:         result.Status.Allowed,
		Denied:          result.Status.Denied,
		Reason:          result.Status.Reason,
		EvaluationError: result.Status.EvaluationError,
	}

	k.logger.V(1).Info("SAR result",
		"user", req.User,
		"allowed", decision.Allowed,
		"denied", decision.Denied,
		"reason", decision.Reason,
	)

	return decision, nil
}

// cachedEntry stores a cached AccessDecision with an expiry time.
type cachedEntry struct {
	decision  AccessDecision
	expiresAt time.Time
}

// CachedAccessChecker wraps an AccessChecker with a TTL-based LRU cache.
type CachedAccessChecker struct {
	delegate   AccessChecker
	cache      *lru.Cache[string, cachedEntry]
	ttl        time.Duration
	logger     logr.Logger
	cacheTotal *prometheus.CounterVec
}

// WithCache wraps an AccessChecker with an in-memory LRU cache.
// Cached decisions expire after ttl. The cache holds at most size entries.
func WithCache(delegate AccessChecker, size int, ttl time.Duration, reg prometheus.Registerer, logger logr.Logger) (*CachedAccessChecker, error) {
	cache, err := lru.New[string, cachedEntry](size)
	if err != nil {
		return nil, fmt.Errorf("creating LRU cache: %w", err)
	}

	cacheTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "authz_cache_lookups_total",
			Help: "Total cache lookups by result (hit, miss, expired).",
		},
		[]string{"result"},
	)
	reg.MustRegister(cacheTotal)

	return &CachedAccessChecker{
		delegate:   delegate,
		cache:      cache,
		ttl:        ttl,
		logger:     logger.WithName("sar-cache"),
		cacheTotal: cacheTotal,
	}, nil
}

// Check looks up the cache first; on miss or expiry, delegates to the
// underlying checker and caches the result.
func (c *CachedAccessChecker) Check(ctx context.Context, req *AccessRequest) (*AccessDecision, error) {
	key := cacheKey(req)

	if entry, ok := c.cache.Get(key); ok {
		if time.Now().Before(entry.expiresAt) {
			c.logger.V(2).Info("cache hit", "user", req.User)
			c.cacheTotal.WithLabelValues("hit").Inc()
			return &entry.decision, nil
		}
		// Expired — remove and proceed to delegate.
		c.cache.Remove(key)
		c.cacheTotal.WithLabelValues("expired").Inc()
	} else {
		c.cacheTotal.WithLabelValues("miss").Inc()
	}

	decision, err := c.delegate.Check(ctx, req)
	if err != nil {
		return nil, err
	}

	c.cache.Add(key, cachedEntry{
		decision:  *decision,
		expiresAt: time.Now().Add(c.ttl),
	})

	return decision, nil
}

// cacheKey produces a deterministic hash of the fields that affect the
// authorization decision. Per the design, the cache key is (oid, namespace)
// since the verb, resource, and API group are fixed from configuration.
func cacheKey(req *AccessRequest) string {
	h := sha256.New()
	fmt.Fprintf(h, "user=%s\n", req.User)
	if req.ResourceAttributes != nil {
		fmt.Fprintf(h, "ns=%s\n", req.ResourceAttributes.Namespace)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}
