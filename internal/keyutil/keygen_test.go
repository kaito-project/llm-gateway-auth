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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateAPIKey(t *testing.T) {
	result, err := GenerateAPIKey("sk-")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(result.RawKey, "sk-"), "expected key to start with sk-, got %s", result.RawKey)
	assert.Equal(t, 8, len(result.KeyID), "expected keyID length 8")
	assert.Len(t, result.Hash, 64, "expected hash length 64")
	assert.True(t, strings.HasPrefix(result.Hash, result.KeyID), "expected hash to start with keyID")
}

func TestGenerateAPIKeyUniqueness(t *testing.T) {
	r1, err := GenerateAPIKey("sk-")
	require.NoError(t, err)
	r2, err := GenerateAPIKey("sk-")
	require.NoError(t, err)
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

func TestExpiryMetadataRoundTrip(t *testing.T) {
	expiresAt := time.Date(2026, time.October, 8, 12, 34, 56, 123456789, time.FixedZone("offset", -7*60*60))
	formatted, err := FormatExpiry(expiresAt)
	require.NoError(t, err)
	assert.Equal(t, "2026-10-08T19:34:56.123456789Z", formatted)

	parsed, err := ParseExpiry(formatted, false)
	require.NoError(t, err)
	assert.Equal(t, expiresAt.UTC(), parsed)
}

func TestPrimaryNeverExpiry(t *testing.T) {
	formatted, err := FormatPrimaryExpiry(nil)
	require.NoError(t, err)
	assert.Equal(t, NeverExpires, formatted)

	parsed, err := ParsePrimaryExpiry(map[string]string{PrimaryExpiryAnnotation: formatted})
	require.NoError(t, err)
	assert.True(t, parsed.IsZero())

	_, err = ParseSecondaryExpiry(map[string]string{SecondaryExpiryAnnotation: NeverExpires})
	assert.ErrorIs(t, err, ErrNeverNotAllowed)
}

func TestExpiryMetadataFailsClosed(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "missing"},
		{name: "malformed", value: "tomorrow"},
		{name: "zero", value: time.Time{}.UTC().Format(time.RFC3339Nano)},
		{name: "non-UTC", value: "2026-10-08T12:00:00-07:00"},
		{name: "non-canonical", value: "2026-10-08T12:00:00.000Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseExpiry(tt.value, false)
			require.Error(t, err)
		})
	}

	_, err := FormatExpiry(time.Time{})
	assert.ErrorIs(t, err, ErrInvalidExpiry)
}
