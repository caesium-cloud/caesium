package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/google/uuid"
)

// Refresh grants are sealed at rest (identity-and-access §11.1, §18):
//
//	key      = HKDF-SHA256(secret = CAESIUM_AUTH_KEY_HASH_SECRET, salt = none,
//	                       info = "caesium/session-refresh/v1", length = 32)
//	envelope = format (1 byte, 0x01) ‖ nonce (12 random bytes) ‖ AES-256-GCM ciphertext+tag
//	AD       = format ‖ user ID (16 bytes)
//
// The salt is deliberately empty (RFC 5869 §2.2 then uses a zero block): the
// secret is an operator-supplied value of at least 32 characters and the info
// string already separates this key from every other use of the secret (the
// session/API-key hashes and the OIDC/SAML state cookies). The derivation and
// the envelope are pinned by known-answer tests; changing either orphans every
// stored grant, so a change needs a new format byte and info string.
//
// The user ID in the additional data binds a grant to its row: a ciphertext
// copied onto another user fails to open. Any failure to open (a rotated
// secret, tampering, a transplant, an unknown format) is ErrRefreshGrantInvalid,
// which callers treat as "no grant" — the spec's "rotating the secret
// invalidates handles".
const (
	refreshGrantKeyInfo  = "caesium/session-refresh/v1"
	refreshGrantFormatV1 = byte(0x01)
	refreshGrantKeyBytes = 32
	refreshGrantNonce    = 12
	// refreshGrantMaxBytes bounds what open will attempt; a real envelope is
	// a few hundred bytes to a few KB (an OIDC refresh token may be a JWT).
	refreshGrantMaxBytes = 64 << 10
)

// ErrRefreshGrantInvalid reports a stored refresh grant that cannot be opened.
// It deliberately carries no detail; callers treat it as "no grant".
var ErrRefreshGrantInvalid = errors.New("refresh grant cannot be opened")

// errRefreshGrantIncomplete rejects sealing a grant with a missing field.
var errRefreshGrantIncomplete = errors.New("refresh grant needs a provider, an issuer and a handle")

const redactedHandle = "[redacted]"

// RefreshHandle is a provider credential that refreshes a user's groups
// without the user present: an OIDC refresh token or an LDAP user DN. It
// redacts itself in every formatted, logged and marshalled form; only Reveal
// returns the value, and only provider code redeeming the grant and the
// sealing code call it.
//
// The value sits behind a closure rather than in a string field: fmt skips
// Format/String while it prints a bad-verb error (say %s over a struct that
// holds a *RefreshGrant) and then walks raw fields by reflection, which
// renders a func only as its address. The func field also makes handles,
// and grants, incomparable, so a token can never be compared with ==.
type RefreshHandle struct {
	reveal func() string
}

// NewRefreshHandle wraps a provider credential. An empty credential is the
// zero handle.
func NewRefreshHandle(secret string) RefreshHandle {
	if secret == "" {
		return RefreshHandle{}
	}
	return RefreshHandle{reveal: func() string { return secret }}
}

// Reveal returns the credential. Never log or return it in an error.
func (h RefreshHandle) Reveal() string {
	if h.reveal == nil {
		return ""
	}
	return h.reveal()
}

// IsZero reports whether the handle is empty.
func (h RefreshHandle) IsZero() bool { return h.Reveal() == "" }

// Equal compares two handles in constant time.
func (h RefreshHandle) Equal(other RefreshHandle) bool {
	return subtle.ConstantTimeCompare([]byte(h.Reveal()), []byte(other.Reveal())) == 1
}

// String redacts the handle.
func (RefreshHandle) String() string { return redactedHandle }

// GoString redacts the handle for %#v.
func (RefreshHandle) GoString() string { return redactedHandle }

// Format redacts the handle for every fmt verb.
func (RefreshHandle) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redactedHandle) }

// MarshalJSON redacts the handle (zap reflects unknown values through JSON).
func (RefreshHandle) MarshalJSON() ([]byte, error) { return json.Marshal(redactedHandle) }

// MarshalText redacts the handle for text encoders.
func (RefreshHandle) MarshalText() ([]byte, error) { return []byte(redactedHandle), nil }

// LogValue redacts the handle for log/slog.
func (RefreshHandle) LogValue() slog.Value { return slog.StringValue(redactedHandle) }

// RefreshGrant is the plaintext of a user's refresh grant.
type RefreshGrant struct {
	// Provider names the configured provider that redeems the grant; it
	// matches that provider's Name() ("oidc" or "ldap").
	Provider string
	// Issuer is the issuer that minted the handle. It must equal the user's
	// issuer, and a provider refuses to redeem a grant from an issuer other
	// than the one it is configured for.
	Issuer string
	// Handle is the credential the provider redeems.
	Handle RefreshHandle
}

func (g RefreshGrant) complete() bool {
	return strings.TrimSpace(g.Provider) != "" && strings.TrimSpace(g.Issuer) != "" && !g.Handle.IsZero()
}

// refreshGrantPayload is the sealed encoding. Handle is a plain string here
// on purpose: RefreshHandle marshals redacted.
type refreshGrantPayload struct {
	Provider string `json:"p"`
	Issuer   string `json:"i"`
	Handle   string `json:"h"`
}

// refreshCipher seals and opens refresh grants for one hash secret.
type refreshCipher struct {
	aead cipher.AEAD
}

func deriveRefreshGrantKey(secret string) ([]byte, error) {
	return hkdf.Key(sha256.New, []byte(secret), nil, refreshGrantKeyInfo, refreshGrantKeyBytes)
}

// newRefreshCipher derives the grant key from secret. An empty secret has no
// cipher: grants are then not stored at all.
func newRefreshCipher(secret string) (*refreshCipher, error) {
	if secret == "" {
		return nil, errors.New("refresh grant secret is empty")
	}
	key, err := deriveRefreshGrantKey(secret)
	if err != nil {
		return nil, fmt.Errorf("derive refresh grant key: %w", err)
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("refresh grant cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("refresh grant cipher: %w", err)
	}
	return &refreshCipher{aead: aead}, nil
}

func refreshGrantAD(format byte, userID uuid.UUID) []byte {
	ad := make([]byte, 0, 1+len(userID))
	ad = append(ad, format)
	return append(ad, userID[:]...)
}

// seal encrypts grant for userID under a fresh random nonce.
func (c *refreshCipher) seal(userID uuid.UUID, grant RefreshGrant) ([]byte, error) {
	nonce := make([]byte, refreshGrantNonce)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("refresh grant nonce: %w", err)
	}
	return c.sealWithNonce(userID, grant, nonce)
}

func (c *refreshCipher) sealWithNonce(userID uuid.UUID, grant RefreshGrant, nonce []byte) ([]byte, error) {
	if c == nil {
		return nil, errors.New("refresh grant cipher is not configured")
	}
	if !grant.complete() {
		return nil, errRefreshGrantIncomplete
	}
	if len(nonce) != refreshGrantNonce {
		return nil, errors.New("refresh grant nonce has the wrong size")
	}
	plaintext, err := json.Marshal(refreshGrantPayload{
		Provider: grant.Provider,
		Issuer:   grant.Issuer,
		Handle:   grant.Handle.Reveal(),
	})
	if err != nil {
		return nil, errors.New("encode refresh grant")
	}
	defer clear(plaintext)
	out := make([]byte, 0, 1+refreshGrantNonce+len(plaintext)+c.aead.Overhead())
	out = append(out, refreshGrantFormatV1)
	out = append(out, nonce...)
	return c.aead.Seal(out, nonce, plaintext, refreshGrantAD(refreshGrantFormatV1, userID)), nil
}

// open decrypts a grant sealed for userID. Every failure is
// ErrRefreshGrantInvalid.
func (c *refreshCipher) open(userID uuid.UUID, sealed []byte) (RefreshGrant, error) {
	if c == nil || len(sealed) < 1+refreshGrantNonce+c.aead.Overhead() || len(sealed) > refreshGrantMaxBytes {
		return RefreshGrant{}, ErrRefreshGrantInvalid
	}
	if sealed[0] != refreshGrantFormatV1 {
		return RefreshGrant{}, ErrRefreshGrantInvalid
	}
	nonce := sealed[1 : 1+refreshGrantNonce]
	plaintext, err := c.aead.Open(nil, nonce, sealed[1+refreshGrantNonce:], refreshGrantAD(sealed[0], userID))
	if err != nil {
		return RefreshGrant{}, ErrRefreshGrantInvalid
	}
	defer clear(plaintext)
	var payload refreshGrantPayload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		return RefreshGrant{}, ErrRefreshGrantInvalid
	}
	grant := RefreshGrant{Provider: payload.Provider, Issuer: payload.Issuer, Handle: NewRefreshHandle(payload.Handle)}
	if !grant.complete() {
		return RefreshGrant{}, ErrRefreshGrantInvalid
	}
	return grant, nil
}
