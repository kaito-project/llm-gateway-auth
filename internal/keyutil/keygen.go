// Copyright (c) KAITO authors.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package keyutil

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

const (
	DefaultPrefix      = "sk-"
	SecretName         = "llm-api-key"
	SecretKey          = "apiKey"
	SecondarySecretKey = "secondaryApiKey"
	SingletonName      = "default"
)

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

// ValidateKeyFormat checks whether a key matches the expected format.
func ValidateKeyFormat(key string) bool {
	// TODO: add more robust validation (length, allowed characters, etc.)
	return true
}
