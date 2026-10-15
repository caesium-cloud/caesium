package oidc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	authpkg "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
)

func TestCompleteEmailVerifiedClaim(t *testing.T) {
	for _, tc := range []struct {
		name  string
		claim any
		want  bool
	}{
		{"absent", nil, false},
		{"false", false, false},
		{"string false", "false", false},
		{"uppercase string", "TRUE", false},
		{"number", 1, false},
		{"object", map[string]any{}, false},
		{"true", true, true},
		{"string true", "true", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issuer := newMockIssuer(t, "groups")
			provider := newTestProvider(t, issuer, "groups")
			issuer.nextEmailVerified = tc.claim

			// A malformed claim must never fail a login that works today.
			identity := completeOIDCCallback(t, provider, issuer)
			require.Equal(t, "ada@example.com", identity.Email)
			require.Equal(t, tc.want, identity.EmailVerified)
		})
	}
}

// TestUnverifiedOIDCEmailCannotClaimEmailOnlyAdminBinding drives the real OIDC
// provider into the shared login tail: an IdP that does not verify the email
// must not reach a users: binding, even when the email string matches.
func TestUnverifiedOIDCEmailCannotClaimEmailOnlyAdminBinding(t *testing.T) {
	p, err := policy.Parse([]byte(`apiVersion: v1
kind: AccessPolicy
bindings:
  - subjects: {users: [ada@example.com]}
    role: admin
    namespaces: ["*"]
`))
	require.NoError(t, err)

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	sso := authpkg.NewSSOService(authpkg.NewUserStore(db), authpkg.NewSessionStore(db), &policy.Static{Policy: p})

	issuer := newMockIssuer(t, "groups")
	provider := newTestProvider(t, issuer, "groups")
	identity := completeOIDCCallback(t, provider, issuer)
	require.False(t, identity.EmailVerified)

	cookie, sess, err := sso.Complete(context.Background(), identity, "oidc", "203.0.113.40", "agent")
	require.ErrorIs(t, err, authpkg.ErrLoginDenied)
	require.Empty(t, cookie)
	require.Nil(t, sess)
	var users int64
	require.NoError(t, db.Model(&models.User{}).Count(&users).Error)
	require.Zero(t, users)

	// Positive control: the same binding admits the IdP-verified email.
	issuer.nextEmailVerified = true
	identity = completeOIDCCallback(t, provider, issuer)
	require.True(t, identity.EmailVerified)
	cookie, _, err = sso.Complete(context.Background(), identity, "oidc", "203.0.113.40", "agent")
	require.NoError(t, err)
	require.NotEmpty(t, cookie)

	var user models.User
	require.NoError(t, db.First(&user, "email = ?", "ada@example.com").Error)
	require.Equal(t, models.RoleAdmin, user.Role)
	require.True(t, user.EmailVerified)
}

func completeOIDCCallback(t *testing.T, provider *Provider, issuer *mockIssuer) *authpkg.ExternalIdentity {
	t.Helper()
	stateCookie, state := beginForCallback(t, provider, "/")
	issuer.nextNonce = state.Nonce
	req := httptest.NewRequestWithContext(context.Background(),
		http.MethodGet,
		"https://app.example.com/auth/sso/oidc/callback?code=good-code&state="+url.QueryEscape(state.State),
		nil,
	)
	req.AddCookie(stateCookie)
	identity, _, err := provider.CompleteWithReturnTo(req)
	require.NoError(t, err)
	return identity
}
