package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	// KeyHashSchemeSHA256 is the legacy unkeyed hash format.
	KeyHashSchemeSHA256 = "sha256"

	// KeyHashSchemeHMACSHA256 is the keyed production hash format.
	KeyHashSchemeHMACSHA256 = "hmac-sha256"
)

// HashKey returns the stored API-key hash string using the keyed scheme when a
// secret is configured, falling back to a versioned legacy SHA-256 hash.
func HashKey(plaintext, secret string) (string, error) {
	if secret == "" {
		return formatStoredHash(KeyHashSchemeSHA256, sha256Hex(plaintext)), nil
	}

	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := mac.Write([]byte(plaintext)); err != nil {
		return "", fmt.Errorf("hash key: %w", err)
	}

	return formatStoredHash(KeyHashSchemeHMACSHA256, hex.EncodeToString(mac.Sum(nil))), nil
}

// HashLookupCandidates returns the stored hash values that may correspond to a
// plaintext key. This keeps validation compatible with legacy rows created
// before hash versioning was introduced.
func HashLookupCandidates(plaintext, secret string) ([]string, error) {
	candidates := make([]string, 0, 3)

	primary, err := HashKey(plaintext, secret)
	if err != nil {
		return nil, err
	}
	candidates = append(candidates, primary)

	legacy := sha256Hex(plaintext)
	legacyPrefixed := formatStoredHash(KeyHashSchemeSHA256, legacy)
	if !containsString(candidates, legacyPrefixed) {
		candidates = append(candidates, legacyPrefixed)
	}
	if !containsString(candidates, legacy) {
		candidates = append(candidates, legacy)
	}

	return candidates, nil
}

func formatStoredHash(scheme, digest string) string {
	return scheme + ":" + digest
}

func sha256Hex(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func hashScheme(stored string) string {
	scheme, _, found := strings.Cut(stored, ":")
	if !found {
		return ""
	}
	return scheme
}
