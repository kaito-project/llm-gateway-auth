package authz

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNamespaceFromHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		host string
		want string
	}{
		{"team-alpha.mgr.example.com", "team-alpha"},
		{"team-alpha.mgr.example.com:8080", "team-alpha"},
		{"production.mgr.example.com:443", "production"},
		{"localhost", ""},
		{"", ""},
		{"localhost:8080", ""},
		{".example.com", ""},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, NamespaceFromHost(tt.host))
		})
	}
}
