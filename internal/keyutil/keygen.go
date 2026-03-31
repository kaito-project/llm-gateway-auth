package keyutil

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
)

const (
	DefaultPrefix     = "sk-"
	SecretName        = "llm-api-key"
	SecretKey         = "apiKey"
	PreviousSecretKey = "previousApiKey"
	SingletonName     = "default"
)

var keyPattern = regexp.MustCompile(`^sk-[A-Za-z0-9_-]{30,}$`)

// GenerateResult holds the output of key generation.
type GenerateResult struct {
	RawKey string
	KeyID  string
	Hash   string
}

// GenerateAPIKey generates a new API key with the given prefix.
// The prefix is the literal beginning of the key (e.g. "sk-" -> "sk-<random>").
// TODO: add checksum
func GenerateAPIKey(prefix string) (*GenerateResult, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("failed to generate random bytes: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	rawKey := prefix + token

	h := sha256.Sum256([]byte(rawKey))
	hashHex := hex.EncodeToString(h[:])
	keyID := hex.EncodeToString(h[:4])

	return &GenerateResult{
		RawKey: rawKey,
		KeyID:  keyID,
		Hash:   hashHex,
	}, nil
}

// HashKey computes the hex-encoded SHA-256 hash of a raw key.
func HashKey(rawKey string) string {
	h := sha256.Sum256([]byte(rawKey))
	return hex.EncodeToString(h[:])
}

// KeyIDFromHash returns the first 8 hex characters of a hash.
func KeyIDFromHash(hashHex string) string {
	if len(hashHex) >= 8 {
		return hashHex[:8]
	}
	return hashHex
}
