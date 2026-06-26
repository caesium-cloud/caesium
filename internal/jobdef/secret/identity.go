package secret

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// IdentityKeyring holds versioned server-side keys used only for baseline
// secret identity HMACs. The key id, not the key bytes, is persisted.
type IdentityKeyring struct {
	currentKeyID string
	keys         map[string][]byte
}

// NewIdentityKeyring validates and builds a versioned identity-HMAC keyring.
func NewIdentityKeyring(currentKeyID string, keys map[string][]byte) (*IdentityKeyring, error) {
	currentKeyID = strings.TrimSpace(currentKeyID)
	if len(keys) == 0 {
		return nil, nil
	}
	copied := make(map[string][]byte, len(keys))
	for id, key := range keys {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, errors.New("secret identity key id cannot be empty")
		}
		if len(key) == 0 {
			return nil, fmt.Errorf("secret identity key %q cannot be empty", id)
		}
		copied[id] = append([]byte(nil), key...)
	}
	if currentKeyID == "" {
		if len(copied) == 1 {
			for id := range copied {
				currentKeyID = id
			}
		} else {
			return nil, errors.New("secret identity current key id is required when multiple keys are configured")
		}
	}
	if _, ok := copied[currentKeyID]; !ok {
		return nil, fmt.Errorf("secret identity current key %q is not in the keyring", currentKeyID)
	}
	return &IdentityKeyring{currentKeyID: currentKeyID, keys: copied}, nil
}

// ParseIdentityKeyring parses a comma-separated id=value list. Values may be
// raw strings, base64:<data>, or hex:<data>.
func ParseIdentityKeyring(currentKeyID, raw string) (*IdentityKeyring, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	keys := map[string][]byte{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, value, ok := strings.Cut(part, "=")
		if !ok {
			id, value, ok = strings.Cut(part, ":")
		}
		if !ok {
			return nil, fmt.Errorf("secret identity key %q must be id=value", part)
		}
		id = strings.TrimSpace(id)
		value = strings.TrimSpace(value)
		key, err := decodeIdentityKey(value)
		if err != nil {
			return nil, fmt.Errorf("secret identity key %q: %w", id, err)
		}
		keys[id] = key
	}
	return NewIdentityKeyring(currentKeyID, keys)
}

func decodeIdentityKey(value string) ([]byte, error) {
	if strings.HasPrefix(value, "base64:") {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, "base64:"))
		if err != nil {
			return nil, err
		}
		return decoded, nil
	}
	if strings.HasPrefix(value, "hex:") {
		decoded, err := hex.DecodeString(strings.TrimPrefix(value, "hex:"))
		if err != nil {
			return nil, err
		}
		return decoded, nil
	}
	return []byte(value), nil
}

func (k *IdentityKeyring) CurrentHMAC(value []byte) (keyID, digest string, ok bool) {
	if k == nil || k.currentKeyID == "" {
		return "", "", false
	}
	digest, ok = k.HMACWithKeyID(k.currentKeyID, value)
	if !ok {
		return "", "", false
	}
	return k.currentKeyID, digest, true
}

func (k *IdentityKeyring) HMACWithKeyID(keyID string, value []byte) (digest string, ok bool) {
	if k == nil {
		return "", false
	}
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		return "", false
	}
	key, ok := k.keys[keyID]
	if !ok || len(key) == 0 {
		return "", false
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(value)
	return hex.EncodeToString(mac.Sum(nil)), true
}
