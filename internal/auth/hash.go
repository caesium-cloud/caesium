package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

const (
	// KeyHashSchemeSHA256 is the unkeyed hash format (used when no secret is configured).
	KeyHashSchemeSHA256 = "sha256"

	// KeyHashSchemeHMACSHA256 is the keyed production hash format.
	KeyHashSchemeHMACSHA256 = "hmac-sha256"
)

// HashKey returns the stored API-key hash string: HMAC-SHA256 when a secret is
// configured, plain SHA-256 otherwise.
func HashKey(plaintext, secret string) (string, error) {
	if secret == "" {
		sum := sha256.Sum256([]byte(plaintext))
		return KeyHashSchemeSHA256 + ":" + hex.EncodeToString(sum[:]), nil
	}

	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := mac.Write([]byte(plaintext)); err != nil {
		return "", fmt.Errorf("hash key: %w", err)
	}

	return KeyHashSchemeHMACSHA256 + ":" + hex.EncodeToString(mac.Sum(nil)), nil
}
