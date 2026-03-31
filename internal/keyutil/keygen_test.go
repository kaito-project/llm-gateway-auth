package keyutil

import (
	"strings"
	"testing"
)

func TestGenerateAPIKey(t *testing.T) {
	result, err := GenerateAPIKey("sk-")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(result.RawKey, "sk-") {
		t.Errorf("expected key to start with sk-, got %s", result.RawKey)
	}
	if len(result.KeyID) != 8 {
		t.Errorf("expected keyID length 8, got %d", len(result.KeyID))
	}
	if len(result.Hash) != 64 {
		t.Errorf("expected hash length 64, got %d", len(result.Hash))
	}
	if result.Hash[:8] != result.KeyID {
		t.Errorf("expected keyID %s to be prefix of hash %s", result.KeyID, result.Hash)
	}
}

func TestGenerateAPIKeyUniqueness(t *testing.T) {
	r1, _ := GenerateAPIKey("sk-")
	r2, _ := GenerateAPIKey("sk-")
	if r1.RawKey == r2.RawKey {
		t.Error("expected different keys")
	}
	if r1.Hash == r2.Hash {
		t.Error("expected different hashes")
	}
}

func TestGenerateAPIKeyWithCustomPrefix(t *testing.T) {
	result, err := GenerateAPIKey("sk-myapp-")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(result.RawKey, "sk-myapp-") {
		t.Errorf("expected key to start with sk-myapp-, got %s", result.RawKey)
	}
}

func TestHashKey(t *testing.T) {
	hash := HashKey("sk-llmgw-test")
	if len(hash) != 64 {
		t.Errorf("expected hash length 64, got %d", len(hash))
	}
	hash2 := HashKey("sk-llmgw-test")
	if hash != hash2 {
		t.Error("expected deterministic hash")
	}
	hash3 := HashKey("sk-llmgw-other")
	if hash == hash3 {
		t.Error("expected different hash for different input")
	}
}

func TestKeyIDFromHash(t *testing.T) {
	hash := "a3f7c2913456789abcdef0123456789abcdef0123456789abcdef0123456789a"
	keyID := KeyIDFromHash(hash)
	if keyID != "a3f7c291" {
		t.Errorf("expected a3f7c291, got %s", keyID)
	}
}
