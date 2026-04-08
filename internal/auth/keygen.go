package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
)

const (
	// KeyPrefixLive is the scannable prefix for production API keys.
	KeyPrefixLive = "csk_live_"

	// randomBytes is the number of random bytes used in key generation (32 bytes = 256 bits).
	randomBytes = 32

	// displayPrefixLen is the length of the key_prefix column value (e.g. "csk_live_a1b2").
	displayPrefixLen = 13
)

// base62 alphabet for URL-safe, scannable key encoding.
const base62Chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// GenerateKey produces a new API key and its SHA-256 hash.
// Returns (plaintext_key, key_prefix, key_hash, error).
// The plaintext key must be shown exactly once at creation time.
func GenerateKey() (plaintext, prefix, hash string, err error) {
	random, err := base62Encode(randomBytes)
	if err != nil {
		return "", "", "", fmt.Errorf("generate key: %w", err)
	}

	plaintext = KeyPrefixLive + random
	prefix = plaintext[:displayPrefixLen]
	hash = HashKey(plaintext)
	return plaintext, prefix, hash, nil
}

// HashKey returns the hex-encoded SHA-256 hash of a plaintext API key.
func HashKey(plaintext string) string {
	h := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(h[:])
}

// base62Encode generates n random bytes and encodes them in base62.
func base62Encode(n int) (string, error) {
	b := make([]byte, 0, n+8) // base62 output is slightly longer than input bytes
	radix := big.NewInt(int64(len(base62Chars)))

	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}

	num := new(big.Int).SetBytes(raw)
	zero := big.NewInt(0)
	mod := new(big.Int)

	for num.Cmp(zero) > 0 {
		num.DivMod(num, radix, mod)
		b = append(b, base62Chars[mod.Int64()])
	}

	// Pad to ensure minimum length (very unlikely to be needed with 32 bytes).
	for len(b) < n {
		b = append(b, base62Chars[0])
	}

	return string(b), nil
}
