// Package authz implements the Envoy ext_authz gRPC service that delegates
// authorization to the Kubernetes API server via SubjectAccessReview.
package authz

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/types/known/structpb"
	authorizationv1 "k8s.io/api/authorization/v1"
)

const (
	// jwtAuthnFilterName is the Envoy filter metadata key where Istio's
	// RequestAuthentication stores validated JWT claims.
	jwtAuthnFilterName = "envoy.filters.http.jwt_authn"
)

// errNoFilterMetadata indicates that the JWT claims metadata was not found
// in the CheckRequest. This typically means Istio's RequestAuthentication
// did not validate a token for this request.
var errNoFilterMetadata = errors.New("no JWT claims in filter metadata")

// azureADClaims holds the identity claims extracted from a validated
// Azure AD / Entra ID JWT via Istio's filter metadata.
type azureADClaims struct {
	// OID is the Azure object ID — stable, tenant-scoped user identifier.
	OID string
	// PreferredUsername is the user principal name (e.g. "alice@contoso.com").
	PreferredUsername string
	// Name is the display name.
	Name string
	// Groups contains Azure AD group object IDs.
	Groups []string
	// TenantID is the Azure tenant ID from the "tid" claim.
	TenantID string
	// Subject is the "sub" claim.
	Subject string
}

// claimsFromFilterMetadata extracts Azure AD claims from the Envoy filter
// metadata populated by Istio's RequestAuthentication (jwt_authn filter).
//
// The metadata is located at:
//
//	attributes.metadata_context.filter_metadata["envoy.filters.http.jwt_authn"]
func claimsFromFilterMetadata(filterMetadata map[string]*structpb.Struct) (*azureADClaims, error) {
	md, ok := filterMetadata[jwtAuthnFilterName]
	if !ok || md == nil {
		return nil, errNoFilterMetadata
	}

	fields := md.GetFields()
	if len(fields) == 0 {
		return nil, fmt.Errorf("%w: metadata struct is empty", errNoFilterMetadata)
	}

	claims := &azureADClaims{
		OID:               stringField(fields, "oid"),
		PreferredUsername: stringField(fields, "preferred_username"),
		Name:              stringField(fields, "name"),
		TenantID:          stringField(fields, "tid"),
		Subject:           stringField(fields, "sub"),
		Groups:            listField(fields, "groups"),
	}

	if claims.OID == "" && claims.Subject == "" {
		return nil, fmt.Errorf("%w: neither oid nor sub claim present", errNoFilterMetadata)
	}

	return claims, nil
}

// toKubernetesSubject maps Azure AD claims to the fields needed for a
// SubjectAccessReview. The OID is used as the primary Kubernetes user
// identity because it is stable and tenant-scoped, unlike "sub" which
// is per-application.
func toKubernetesSubject(claims *azureADClaims) (user string, groups []string, extra map[string]authorizationv1.ExtraValue) {
	user = claims.OID
	if user == "" {
		// Fallback: use sub if oid is not present (e.g. some v1.0 tokens).
		user = claims.Subject
	}

	groups = claims.Groups

	extra = map[string]authorizationv1.ExtraValue{}
	if claims.OID != "" {
		extra["oid"] = authorizationv1.ExtraValue{claims.OID}
	}
	if claims.PreferredUsername != "" {
		extra["preferred_username"] = authorizationv1.ExtraValue{claims.PreferredUsername}
	}
	if claims.TenantID != "" {
		extra["tid"] = authorizationv1.ExtraValue{claims.TenantID}
	}

	return user, groups, extra
}

// stringField extracts a string value from a protobuf Struct fields map.
func stringField(fields map[string]*structpb.Value, key string) string {
	v, ok := fields[key]
	if !ok || v == nil {
		return ""
	}
	if sv, ok := v.GetKind().(*structpb.Value_StringValue); ok {
		return sv.StringValue
	}
	return ""
}

// listField extracts a list of strings from a protobuf Struct fields map.
func listField(fields map[string]*structpb.Value, key string) []string {
	v, ok := fields[key]
	if !ok || v == nil {
		return nil
	}
	lv, ok := v.GetKind().(*structpb.Value_ListValue)
	if !ok || lv.ListValue == nil {
		return nil
	}
	var result []string
	for _, item := range lv.ListValue.GetValues() {
		if sv, ok := item.GetKind().(*structpb.Value_StringValue); ok {
			result = append(result, sv.StringValue)
		}
	}
	return result
}
