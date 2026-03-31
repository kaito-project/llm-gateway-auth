package authz

// Config holds authorization-related configuration for the ext_authz server.
type Config struct {
	// SARAPIGroup is the API group for SubjectAccessReview ResourceAttributes (e.g. "aimanager").
	SARAPIGroup string
	// SARResource is the resource type for SubjectAccessReview ResourceAttributes (e.g. "namespaces").
	SARResource string
	// SARVerb is the verb for SubjectAccessReview ResourceAttributes (e.g. "inference").
	SARVerb string
	// FailOpen allows requests when authorization infrastructure is unavailable.
	FailOpen bool
}
