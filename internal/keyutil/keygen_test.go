package keyutil

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateAPIKey(t *testing.T) {
	result, err := GenerateAPIKey("sk-")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(result.RawKey, "sk-"), "expected key to start with sk-, got %s", result.RawKey)
	assert.Equal(t, 8, len(result.KeyID), "expected keyID length 8")
	assert.Equal(t, 64, len(result.Hash), "expected hash length 64")
	assert.Equal(t, result.Hash[:8], result.KeyID, "expected keyID to be prefix of hash")
}

func TestGenerateAPIKeyUniqueness(t *testing.T) {
	r1, _ := GenerateAPIKey("sk-")
	r2, _ := GenerateAPIKey("sk-")
	assert.NotEqual(t, r1.RawKey, r2.RawKey, "expected different keys")
	assert.NotEqual(t, r1.Hash, r2.Hash, "expected different hashes")
}

func TestGenerateAPIKeyWithCustomPrefix(t *testing.T) {
	result, err := GenerateAPIKey("sk-myapp-")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(result.RawKey, "sk-myapp-"), "expected key to start with sk-myapp-, got %s", result.RawKey)
}

func TestHashKey(t *testing.T) {
	hash := HashKey("sk-llmgw-test")
	assert.Equal(t, 64, len(hash), "expected hash length 64")

	hash2 := HashKey("sk-llmgw-test")
	assert.Equal(t, hash, hash2, "expected deterministic hash")

	hash3 := HashKey("sk-llmgw-other")
	assert.NotEqual(t, hash, hash3, "expected different hash for different input")
}

func TestKeyIDFromHash(t *testing.T) {
	hash := "a3f7c2913456789abcdef0123456789abcdef0123456789abcdef0123456789a"
	keyID := KeyIDFromHash(hash)
	assert.Equal(t, "a3f7c291", keyID)
}
