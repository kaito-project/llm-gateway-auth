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
	"errors"
	"fmt"
	"time"
)

const (
	DefaultPrefix      = "sk-"
	SecretName         = "llm-api-key"
	SecretKey          = "apiKey"
	SecondarySecretKey = "secondaryApiKey"
	SingletonName      = "default"

	PrimaryExpiryAnnotation   = "kaito.sh/primary-key-expires-at"
	SecondaryExpiryAnnotation = "kaito.sh/secondary-key-expires-at"
	PrimaryHashAnnotation     = "kaito.sh/primary-key-hash"
	SecondaryHashAnnotation   = "kaito.sh/secondary-key-hash"
	RotationRequestAnnotation = "kaito.sh/rotation-request"
	LastRotatedAtAnnotation   = "kaito.sh/last-rotated-at"
	NeverExpires              = "never"
)

var (
	ErrMissingExpiry   = errors.New("expiry metadata is missing")
	ErrInvalidExpiry   = errors.New("expiry metadata is invalid")
	ErrNeverNotAllowed = errors.New("never is not allowed for this expiry")
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

// FormatExpiry formats a finite expiry as a canonical UTC RFC3339Nano value.
func FormatExpiry(expiresAt time.Time) (string, error) {
	if expiresAt.IsZero() {
		return "", fmt.Errorf("%w: zero timestamp", ErrInvalidExpiry)
	}
	return expiresAt.UTC().Format(time.RFC3339Nano), nil
}

// FormatPrimaryExpiry formats a primary expiry. A nil expiry is represented by
// the explicit NeverExpires sentinel; finite zero values are rejected.
func FormatPrimaryExpiry(expiresAt *time.Time) (string, error) {
	if expiresAt == nil {
		return NeverExpires, nil
	}
	return FormatExpiry(*expiresAt)
}

// ParseExpiry parses a canonical finite UTC RFC3339Nano expiry. The never
// sentinel is accepted only when allowNever is true, and is represented by a
// zero time in the returned value.
func ParseExpiry(value string, allowNever bool) (time.Time, error) {
	if value == "" {
		return time.Time{}, ErrMissingExpiry
	}
	if value == NeverExpires {
		if !allowNever {
			return time.Time{}, ErrNeverNotAllowed
		}
		return time.Time{}, nil
	}

	expiresAt, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || expiresAt.IsZero() {
		return time.Time{}, fmt.Errorf("%w: expected a non-zero UTC RFC3339Nano timestamp", ErrInvalidExpiry)
	}
	canonical := expiresAt.UTC().Format(time.RFC3339Nano)
	if value != canonical {
		return time.Time{}, fmt.Errorf("%w: expected canonical UTC value %q", ErrInvalidExpiry, canonical)
	}
	return expiresAt, nil
}

// ParsePrimaryExpiry reads and parses primary expiry metadata from annotations.
func ParsePrimaryExpiry(annotations map[string]string) (time.Time, error) {
	return ParseExpiry(annotations[PrimaryExpiryAnnotation], true)
}

// ParseSecondaryExpiry reads and parses finite secondary expiry metadata from annotations.
func ParseSecondaryExpiry(annotations map[string]string) (time.Time, error) {
	return ParseExpiry(annotations[SecondaryExpiryAnnotation], false)
}

// ValidateKeyFormat checks whether a key matches the expected format.
func ValidateKeyFormat(key string) bool {
	// TODO: add more robust validation (length, allowed characters, etc.)
	return true
}
