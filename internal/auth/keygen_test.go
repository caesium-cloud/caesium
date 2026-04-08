package auth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGenerateKey(t *testing.T) {
	plaintext, prefix, hash, err := GenerateKey()
	require.NoError(t, err)

	// Key starts with the live prefix.
	require.True(t, strings.HasPrefix(plaintext, KeyPrefixLive))

	// Prefix is the first 13 characters of the full key.
	require.Equal(t, plaintext[:displayPrefixLen], prefix)
	require.Len(t, prefix, displayPrefixLen)

	// Hash matches.
	require.Equal(t, HashKey(plaintext), hash)

	// Key has sufficient length (prefix + 32+ encoded chars).
	require.Greater(t, len(plaintext), 30)
}

func TestGenerateKeyUniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		plaintext, _, _, err := GenerateKey()
		require.NoError(t, err)
		require.False(t, seen[plaintext], "duplicate key generated")
		seen[plaintext] = true
	}
}

func TestHashKeyDeterministic(t *testing.T) {
	key := "csk_live_testkey123"
	h1 := HashKey(key)
	h2 := HashKey(key)
	require.Equal(t, h1, h2)
	require.Len(t, h1, 64) // SHA-256 hex = 64 chars
}

func TestHashKeyDifferentInputs(t *testing.T) {
	require.NotEqual(t, HashKey("key1"), HashKey("key2"))
}
