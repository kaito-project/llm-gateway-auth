package authz

import "strings"

// namespaceFromHost extracts the namespace from the first subdomain of
// the Host header. For example:
//
//	"team-alpha.manager-instance-xx.example.com" → "team-alpha"
//	"team-alpha.manager-instance-xx.example.com:8080" → "team-alpha"
//
// Returns empty string if the host has no subdomain.
func NamespaceFromHost(host string) string {
	// Strip port if present.
	if idx := strings.LastIndexByte(host, ':'); idx != -1 {
		// Only strip if it looks like a port (after the last colon, not inside IPv6 brackets).
		if !strings.Contains(host[idx:], "]") {
			host = host[:idx]
		}
	}

	dot := strings.IndexByte(host, '.')
	if dot <= 0 {
		return ""
	}
	return host[:dot]
}
