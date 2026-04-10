package auth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGenerateKey(t *testing.T) {
	plaintext, prefix, err := GenerateKey()
	require.NoError(t, err)

	// Key starts with the live prefix.
	require.True(t, strings.HasPrefix(plaintext, KeyPrefixLive))

	// Prefix is the first 13 characters of the full key.
	require.Equal(t, plaintext[:displayPrefixLen], prefix)
	require.Len(t, prefix, displayPrefixLen)

	// Key has sufficient length (prefix + 32+ encoded chars).
	require.Greater(t, len(plaintext), 30)
}

func TestGenerateKeyUniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		plaintext, _, err := GenerateKey()
		require.NoError(t, err)
		require.False(t, seen[plaintext], "duplicate key generated")
		seen[plaintext] = true
	}
}

func TestHashKeyDeterministicWithoutSecret(t *testing.T) {
	key := "csk_live_testkey123"
	h1, err := HashKey(key, "")
	require.NoError(t, err)
	h2, err := HashKey(key, "")
	require.NoError(t, err)
	require.Equal(t, h1, h2)
	require.Equal(t, "sha256:80e63ac72c6d9aaadb750de82a4b0f7e606133db9a0264559eaecb7789465029", h1)
}

func TestHashKeyUsesHMACWhenSecretConfigured(t *testing.T) {
	h, err := HashKey("csk_live_testkey123", "super-secret")
	require.NoError(t, err)
	require.Equal(t, "hmac-sha256:99620ba47041b7576ac9c72874fc81913345ce1f3aa2cbeec28c5aa65d79e20c", h)
}

