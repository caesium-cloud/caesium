package auth

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The known answers below were computed independently of this package
// (Python: hmac/hashlib HKDF-SHA256 and cryptography's AESGCM), so they pin
// the key derivation, the empty salt, the info string, the envelope layout,
// the additional data and the payload encoding.
const (
	katSecret   = "caesium-refresh-kat-secret-0123456789abcdef"
	katKeyHex   = "c70ab4afe07e3c5a57a3cc25b807ad8f94c211d1a53506c1225df1c9125a9fef"
	katUserID   = "6f1c2a3b-4d5e-4f60-8a71-92b3c4d5e6f7"
	katEnvelope = "01000102030405060708090a0b58ff3c4a527b62aeadacdffecb282fd5dc91281738ff6c4533eccc580c2397d4be2d20a8720cd0861b447250a57698b59321159ff75ca845aa84b3da92736e23ef36fc08276fe4b4448c7222dc440250be14f68209e3dcc63a935e8a6adac6fd5b"
)

var katGrant = RefreshGrant{
	Provider: "oidc",
	Issuer:   "https://idp.example.com/realms/caesium",
	Handle:   NewRefreshHandle("kat-refresh-token"),
}

func testRefreshCipher(t *testing.T, secret string) *refreshCipher {
	t.Helper()
	c, err := newRefreshCipher(secret)
	require.NoError(t, err)
	return c
}

func TestRefreshGrantKeyDerivationKnownAnswer(t *testing.T) {
	key, err := deriveRefreshGrantKey(katSecret)
	require.NoError(t, err)
	require.Equal(t, katKeyHex, hex.EncodeToString(key))
}

func TestRefreshGrantEnvelopeKnownAnswer(t *testing.T) {
	c := testRefreshCipher(t, katSecret)
	userID := uuid.MustParse(katUserID)
	envelope, err := hex.DecodeString(katEnvelope)
	require.NoError(t, err)

	grant, err := c.open(userID, envelope)
	require.NoError(t, err)
	require.Equal(t, katGrant.Provider, grant.Provider)
	require.Equal(t, katGrant.Issuer, grant.Issuer)
	require.Equal(t, katGrant.Handle.Reveal(), grant.Handle.Reveal())

	nonce := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	sealed, err := c.sealWithNonce(userID, katGrant, nonce)
	require.NoError(t, err)
	require.Equal(t, katEnvelope, hex.EncodeToString(sealed))
}

func TestRefreshGrantRoundTrip(t *testing.T) {
	c := testRefreshCipher(t, katSecret)
	userID := uuid.New()
	for _, grant := range []RefreshGrant{
		katGrant,
		{Provider: "ldap", Issuer: "ldap", Handle: NewRefreshHandle("uid=al(ice),ou=People,dc=example,dc=com"), Login: "alice@example.com"},
		{Provider: "oidc", Issuer: "https://idp", Handle: NewRefreshHandle(strings.Repeat("jwt.", 1000))},
	} {
		sealed, err := c.seal(userID, grant)
		require.NoError(t, err)
		require.Equal(t, refreshGrantFormatV1, sealed[0])
		require.False(t, bytes.Contains(sealed, []byte(grant.Handle.Reveal())), "ciphertext must not contain the handle")
		got, err := c.open(userID, sealed)
		require.NoError(t, err)
		require.Equal(t, grant.Provider, got.Provider)
		require.Equal(t, grant.Issuer, got.Issuer)
		require.True(t, grant.Handle.Equal(got.Handle))
		require.Equal(t, grant.Login, got.Login)
	}
}

func TestRefreshGrantTamperFails(t *testing.T) {
	c := testRefreshCipher(t, katSecret)
	userID := uuid.New()
	sealed, err := c.seal(userID, katGrant)
	require.NoError(t, err)

	parts := map[string]int{
		"format":     0,
		"nonce":      1,
		"nonce end":  refreshGrantNonce,
		"ciphertext": 1 + refreshGrantNonce,
		"tag":        len(sealed) - 1,
	}
	for name, index := range parts {
		t.Run(name, func(t *testing.T) {
			tampered := bytes.Clone(sealed)
			tampered[index] ^= 0x01
			_, err := c.open(userID, tampered)
			require.ErrorIs(t, err, ErrRefreshGrantInvalid)
		})
	}
	t.Run("truncated", func(t *testing.T) {
		for _, n := range []int{0, 1, 1 + refreshGrantNonce, len(sealed) - 1} {
			_, err := c.open(userID, sealed[:n])
			require.ErrorIs(t, err, ErrRefreshGrantInvalid)
		}
	})
	t.Run("extended", func(t *testing.T) {
		_, err := c.open(userID, append(bytes.Clone(sealed), 0))
		require.ErrorIs(t, err, ErrRefreshGrantInvalid)
	})
	t.Run("oversized", func(t *testing.T) {
		_, err := c.open(userID, make([]byte, refreshGrantMaxBytes+1))
		require.ErrorIs(t, err, ErrRefreshGrantInvalid)
	})
}

func TestRefreshGrantTransplantToAnotherUserFails(t *testing.T) {
	c := testRefreshCipher(t, katSecret)
	owner, other := uuid.New(), uuid.New()
	sealed, err := c.seal(owner, katGrant)
	require.NoError(t, err)
	_, err = c.open(other, sealed)
	require.ErrorIs(t, err, ErrRefreshGrantInvalid)
	_, err = c.open(owner, sealed)
	require.NoError(t, err)
}

func TestRefreshGrantWrongSecretFails(t *testing.T) {
	userID := uuid.New()
	sealed, err := testRefreshCipher(t, katSecret).seal(userID, katGrant)
	require.NoError(t, err)
	_, err = testRefreshCipher(t, katSecret+"-rotated").open(userID, sealed)
	require.ErrorIs(t, err, ErrRefreshGrantInvalid)

	var noCipher *refreshCipher
	_, err = noCipher.open(userID, sealed)
	require.ErrorIs(t, err, ErrRefreshGrantInvalid)
}

func TestRefreshGrantUnknownFormatFails(t *testing.T) {
	c := testRefreshCipher(t, katSecret)
	userID := uuid.New()
	sealed, err := c.seal(userID, katGrant)
	require.NoError(t, err)
	for _, format := range []byte{0x00, 0x02, 0xff} {
		changed := bytes.Clone(sealed)
		changed[0] = format
		_, err := c.open(userID, changed)
		require.ErrorIs(t, err, ErrRefreshGrantInvalid)
	}

	// A well-formed ciphertext under another format byte fails too: the
	// format is part of the additional data, not only a prefix check.
	nonce := make([]byte, refreshGrantNonce)
	plaintext, err := json.Marshal(refreshGrantPayload{Provider: "oidc", Issuer: "i", Handle: "h"})
	require.NoError(t, err)
	forged := append([]byte{0x02}, nonce...)
	forged = c.aead.Seal(forged, nonce, plaintext, refreshGrantAD(0x02, userID))
	_, err = c.open(userID, forged)
	require.ErrorIs(t, err, ErrRefreshGrantInvalid)
}

func TestRefreshGrantNoncesDiffer(t *testing.T) {
	c := testRefreshCipher(t, katSecret)
	userID := uuid.New()
	seen := map[string]bool{}
	for range 64 {
		sealed, err := c.seal(userID, katGrant)
		require.NoError(t, err)
		nonce := string(sealed[1 : 1+refreshGrantNonce])
		require.False(t, seen[nonce], "nonce reused")
		seen[nonce] = true
	}
}

func TestRefreshGrantRejectsIncompleteGrants(t *testing.T) {
	c := testRefreshCipher(t, katSecret)
	for _, grant := range []RefreshGrant{
		{Issuer: "i", Handle: NewRefreshHandle("h")},
		{Provider: "oidc", Handle: NewRefreshHandle("h")},
		{Provider: "oidc", Issuer: "i"},
		{Provider: " ", Issuer: "i", Handle: NewRefreshHandle("h")},
	} {
		_, err := c.seal(uuid.New(), grant)
		require.Error(t, err)
	}
	_, err := newRefreshCipher("")
	require.Error(t, err)
}

func TestRefreshHandleRedactsEverywhere(t *testing.T) {
	const token = "super-secret-refresh-token"
	handle := NewRefreshHandle(token)
	grant := RefreshGrant{Provider: "oidc", Issuer: "https://idp", Handle: handle}
	ext := &ExternalIdentity{Issuer: "https://idp", Subject: "sub", Email: "a@b.c", Refresh: &grant}

	var rendered []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		rendered = append(rendered,
			fmt.Sprintf(verb, handle),
			fmt.Sprintf(verb, grant),
			fmt.Sprintf(verb, &grant),
			fmt.Sprintf(verb, ext),
			fmt.Sprintf(verb, *ext),
		)
	}
	for _, v := range []any{handle, grant, ext, *ext, []RefreshGrant{grant}} {
		raw, err := json.Marshal(v)
		require.NoError(t, err)
		rendered = append(rendered, string(raw))
	}
	text, err := handle.MarshalText()
	require.NoError(t, err)
	rendered = append(rendered, string(text), handle.LogValue().String())

	var slogBuf bytes.Buffer
	slog.New(slog.NewJSONHandler(&slogBuf, nil)).Info("refresh", "handle", handle, "grant", grant, "identity", ext)
	rendered = append(rendered, slogBuf.String())

	core, logs := observer.New(zapcore.DebugLevel)
	zap.New(core).Sugar().Infow("refresh", "handle", handle, "grant", grant, "identity", ext)
	for _, entry := range logs.All() {
		var buf bytes.Buffer
		enc := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
		out, err := enc.EncodeEntry(entry.Entry, entry.Context)
		require.NoError(t, err)
		buf.Write(out.Bytes())
		rendered = append(rendered, buf.String())
	}

	for _, s := range rendered {
		require.NotContains(t, s, token)
	}
	require.Equal(t, token, handle.Reveal())
}

func TestRefreshErrorClassification(t *testing.T) {
	require.Zero(t, ClassifyRefreshError(nil))
	require.Equal(t, RefreshUncertain, ClassifyRefreshError(errors.New("unclassified")), "unknown failures must never revoke")
	require.Equal(t, RefreshUncertain, ClassifyRefreshError(&RefreshError{}), "a kindless error is uncertain")
	transient := NewRefreshError(RefreshTransient, "not_sent", errors.New("dial"))
	require.Equal(t, RefreshTransient, ClassifyRefreshError(fmt.Errorf("wrapped: %w", transient)))
	denial := NewRefreshDenial(DenialOfSubject, "user_not_found")
	require.Equal(t, RefreshDefinitive, ClassifyRefreshError(denial))
	require.Equal(t, DenialOfSubject, denial.Scope)
	require.Equal(t, "group refresh transient: not_sent: dial", transient.Error())
	require.ErrorContains(t, transient, "dial")
	for kind, label := range map[RefreshErrorKind]string{
		RefreshTransient: "transient", RefreshUncertain: "uncertain", RefreshDefinitive: "definitive", 0: "unknown",
	} {
		require.Equal(t, label, kind.String())
	}
}
