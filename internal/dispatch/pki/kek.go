package pki

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
)

const (
	caKEKInfo    = "caesium-internal-mtls-ca-kek-v1"
	csrMACInfo   = "caesium-internal-mtls-csr-mac-v1"
	derivedBytes = 32
	gcmNonceSize = 12
)

// DerivedKeys are independent HKDF-SHA256 outputs derived from the operator's
// shared internal token. The raw token is never used for CA encryption or CSR
// authentication.
type DerivedKeys struct {
	CAKEK  []byte
	CSRMac []byte
}

// DeriveKeys expands token into the CA-key encryption key and CSR MAC key.
func DeriveKeys(token string) (DerivedKeys, error) {
	if token == "" {
		return DerivedKeys{}, fmt.Errorf("pki: empty internal mTLS token")
	}
	kek, err := hkdf.Key(sha256.New, []byte(token), nil, caKEKInfo, derivedBytes)
	if err != nil {
		return DerivedKeys{}, fmt.Errorf("pki: derive CA KEK: %w", err)
	}
	mac, err := hkdf.Key(sha256.New, []byte(token), nil, csrMACInfo, derivedBytes)
	if err != nil {
		return DerivedKeys{}, fmt.Errorf("pki: derive CSR MAC key: %w", err)
	}
	return DerivedKeys{CAKEK: kek, CSRMac: mac}, nil
}

// SealCAKey encrypts a PEM-encoded CA private key under kek using AES-256-GCM.
func SealCAKey(kek, plaintext []byte) (ciphertext, nonce []byte, err error) {
	aead, err := caKeyAEAD(kek)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, gcmNonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("pki: generate CA key nonce: %w", err)
	}
	return aead.Seal(nil, nonce, plaintext, nil), nonce, nil
}

// OpenCAKey decrypts a PEM-encoded CA private key sealed by SealCAKey.
func OpenCAKey(kek, ciphertext, nonce []byte) ([]byte, error) {
	if len(nonce) != gcmNonceSize {
		return nil, fmt.Errorf("pki: invalid CA key nonce length %d", len(nonce))
	}
	aead, err := caKeyAEAD(kek)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("pki: open CA key: %w", err)
	}
	return plaintext, nil
}

func caKeyAEAD(kek []byte) (cipher.AEAD, error) {
	if len(kek) != derivedBytes {
		return nil, fmt.Errorf("pki: CA KEK must be %d bytes, got %d", derivedBytes, len(kek))
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, fmt.Errorf("pki: init CA key cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("pki: init CA key GCM: %w", err)
	}
	return aead, nil
}
