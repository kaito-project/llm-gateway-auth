package authz

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// sarReactor returns a k8s reactor that responds to SubjectAccessReview
// creates with the given status.
func sarReactor(allowed, denied bool, reason string) func(k8stesting.Action) (bool, runtime.Object, error) {
	return func(action k8stesting.Action) (bool, runtime.Object, error) {
		sar := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		sar.Status = authorizationv1.SubjectAccessReviewStatus{
			Allowed: allowed,
			Denied:  denied,
			Reason:  reason,
		}
		return true, sar, nil
	}
}

// sarErrorReactor returns a reactor that fails with the given error.
func sarErrorReactor(err error) func(k8stesting.Action) (bool, runtime.Object, error) {
	return func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, err
	}
}

func testAccessRequest() *AccessRequest {
	return &AccessRequest{
		User:   "test-user",
		Groups: []string{"group-a"},
		ResourceAttributes: &authorizationv1.ResourceAttributes{
			Namespace: "team-alpha",
			Verb:      "inference",
			Group:     "aimanager",
			Resource:  "namespaces",
		},
	}
}

func TestKubernetesAccessChecker_Allowed(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset()
	client.PrependReactor("create", "subjectaccessreviews", sarReactor(true, false, ""))

	checker := NewKubernetesAccessChecker(client, logr.Discard())
	decision, err := checker.Check(context.Background(), testAccessRequest())
	require.NoError(t, err)

	assert.True(t, decision.Allowed)
	assert.False(t, decision.Denied)
}

func TestKubernetesAccessChecker_Denied(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset()
	client.PrependReactor("create", "subjectaccessreviews", sarReactor(false, false, "no policy matched"))

	checker := NewKubernetesAccessChecker(client, logr.Discard())
	decision, err := checker.Check(context.Background(), testAccessRequest())
	require.NoError(t, err)

	assert.False(t, decision.Allowed)
	assert.Equal(t, "no policy matched", decision.Reason)
}

func TestKubernetesAccessChecker_ExplicitDeny(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset()
	client.PrependReactor("create", "subjectaccessreviews", sarReactor(false, true, "explicitly denied"))

	checker := NewKubernetesAccessChecker(client, logr.Discard())
	decision, err := checker.Check(context.Background(), testAccessRequest())
	require.NoError(t, err)

	assert.False(t, decision.Allowed)
	assert.True(t, decision.Denied)
	assert.Equal(t, "explicitly denied", decision.Reason)
}

func TestKubernetesAccessChecker_APIError(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset()
	client.PrependReactor("create", "subjectaccessreviews", sarErrorReactor(errors.New("connection refused")))

	checker := NewKubernetesAccessChecker(client, logr.Discard())
	_, err := checker.Check(context.Background(), testAccessRequest())
	assert.Error(t, err)
}

func TestKubernetesAccessChecker_ResourceAttributesForwarded(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset()

	var capturedSAR *authorizationv1.SubjectAccessReview
	client.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		sar := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		capturedSAR = sar.DeepCopy()
		sar.Status.Allowed = true
		return true, sar, nil
	})

	checker := NewKubernetesAccessChecker(client, logr.Discard())
	_, err := checker.Check(context.Background(), testAccessRequest())
	require.NoError(t, err)
	require.NotNil(t, capturedSAR)

	spec := capturedSAR.Spec
	assert.Equal(t, "test-user", spec.User)

	ra := spec.ResourceAttributes
	require.NotNil(t, ra)
	assert.Equal(t, "team-alpha", ra.Namespace)
	assert.Equal(t, "inference", ra.Verb)
	assert.Equal(t, "aimanager", ra.Group)
	assert.Equal(t, "namespaces", ra.Resource)
}

// mockChecker is a hand-written mock for AccessChecker.
type mockChecker struct {
	decision *AccessDecision
	err      error
	calls    int
}

func (m *mockChecker) Check(_ context.Context, _ *AccessRequest) (*AccessDecision, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return m.decision, nil
}

func TestCachedAccessChecker_Hit(t *testing.T) {
	t.Parallel()
	mock := &mockChecker{decision: &AccessDecision{Allowed: true}}
	cached, err := WithCache(mock, 10, 1*time.Minute, prometheus.NewRegistry(), logr.Discard())
	require.NoError(t, err)

	req := testAccessRequest()

	// First call — cache miss.
	d1, err := cached.Check(context.Background(), req)
	require.NoError(t, err)
	assert.True(t, d1.Allowed)

	// Second call — cache hit (delegate not called again).
	d2, err := cached.Check(context.Background(), req)
	require.NoError(t, err)
	assert.True(t, d2.Allowed)

	assert.Equal(t, 1, mock.calls, "delegate should be called once; second call should be cached")
}

func TestCachedAccessChecker_Expiry(t *testing.T) {
	t.Parallel()
	mock := &mockChecker{decision: &AccessDecision{Allowed: true}}
	cached, err := WithCache(mock, 10, 1*time.Millisecond, prometheus.NewRegistry(), logr.Discard())
	require.NoError(t, err)

	req := testAccessRequest()

	_, _ = cached.Check(context.Background(), req)
	time.Sleep(5 * time.Millisecond)
	_, _ = cached.Check(context.Background(), req)

	assert.Equal(t, 2, mock.calls, "delegate should be called twice after TTL expiry")
}

func TestCachedAccessChecker_DifferentRequests(t *testing.T) {
	t.Parallel()
	mock := &mockChecker{decision: &AccessDecision{Allowed: true}}
	cached, err := WithCache(mock, 10, 1*time.Minute, prometheus.NewRegistry(), logr.Discard())
	require.NoError(t, err)

	req1 := testAccessRequest()
	req2 := testAccessRequest()
	req2.User = "other-user"

	_, _ = cached.Check(context.Background(), req1)
	_, _ = cached.Check(context.Background(), req2)

	assert.Equal(t, 2, mock.calls, "different requests should miss cache")
}

func TestCacheKey_Deterministic(t *testing.T) {
	t.Parallel()
	req := testAccessRequest()
	assert.Equal(t, cacheKey(req), cacheKey(req))
}

func TestCacheKey_DifferentInputs(t *testing.T) {
	t.Parallel()
	req1 := testAccessRequest()
	req2 := testAccessRequest()
	req2.User = "different-user"
	assert.NotEqual(t, cacheKey(req1), cacheKey(req2))
}

func TestCacheKey_DifferentNamespace(t *testing.T) {
	t.Parallel()
	req1 := testAccessRequest()
	req2 := testAccessRequest()
	req2.ResourceAttributes.Namespace = "team-beta"
	assert.NotEqual(t, cacheKey(req1), cacheKey(req2))
}

func TestCacheKey_SameUserNamespace_DifferentGroups(t *testing.T) {
	t.Parallel()
	req1 := testAccessRequest()
	req2 := testAccessRequest()
	req2.Groups = []string{"group-b", "group-c"}
	assert.Equal(t, cacheKey(req1), cacheKey(req2), "cache key should only depend on user and namespace")
}
