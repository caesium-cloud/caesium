package oidc

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	authpkg "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	storedRefreshToken  = "stored-refresh-token-2f6c1d"
	rotatedRefreshToken = "rotated-refresh-token-9a4e7b"
	testHashSecret      = "oidc-refresh-test-secret-0123456789abcdef"
)

// refresh serves the refresh_token grant from the mock issuer.
func (m *mockIssuer) refresh(w http.ResponseWriter, r *http.Request) {
	m.refreshCalls.Add(1)
	m.lastRefreshForm = cloneValues(r.PostForm)
	m.lastRefreshAuth = r.Header.Get("Authorization")
	if m.onRefresh != nil && m.onRefresh(w, r) {
		return
	}
	claims := map[string]any{
		"iss":         m.URL(),
		"aud":         "caesium",
		"sub":         "subject-123",
		"exp":         time.Now().Add(time.Hour).Unix(),
		"iat":         time.Now().Add(-time.Minute).Unix(),
		"email":       "ada@example.com",
		m.groupsClaim: []string{"data-eng", "ops"},
	}
	if m.refreshClaims != nil {
		m.refreshClaims(claims)
	}
	response := map[string]any{
		"access_token": "refreshed-access-token",
		"token_type":   "Bearer",
		"expires_in":   3600,
	}
	if !m.refreshOmitIDToken {
		key := m.key
		if m.refreshSigningKey != nil {
			key = m.refreshSigningKey
		}
		response["id_token"] = signJWT(key, claims)
	}
	if m.refreshRotateTo != "" {
		response["refresh_token"] = m.refreshRotateTo
	}
	writeJSON(w, response)
}

func refreshRequestFor(issuer *mockIssuer, token string) authpkg.RefreshRequest {
	return authpkg.RefreshRequest{
		UserID:  uuid.New(),
		Issuer:  issuer.URL(),
		Subject: "subject-123",
		Grant:   authpkg.RefreshGrant{Provider: ProviderName, Issuer: issuer.URL(), Handle: authpkg.NewRefreshHandle(token)},
	}
}

func requireRefreshError(t *testing.T, err error, kind authpkg.RefreshErrorKind, reason string) *authpkg.RefreshError {
	t.Helper()
	require.Error(t, err)
	refreshErr, ok := err.(*authpkg.RefreshError)
	require.Truef(t, ok, "want *RefreshError, got %T: %v", err, err)
	assert.Equal(t, kind, refreshErr.Kind, "kind of %v", err)
	if reason != "" {
		assert.Equal(t, reason, refreshErr.Reason)
	}
	assert.Equal(t, kind, authpkg.ClassifyRefreshError(err))
	for _, secret := range []string{storedRefreshToken, rotatedRefreshToken, "refreshed-access-token", "client-secret"} {
		assert.NotContains(t, err.Error(), secret)
	}
	return refreshErr
}

// requireNoTokenInLogs scans the process log ring for any token.
func requireNoTokenInLogs(t *testing.T) {
	t.Helper()
	for _, entry := range log.Buffer().Snapshot() {
		raw, err := json.Marshal(entry)
		require.NoError(t, err)
		for _, secret := range []string{storedRefreshToken, rotatedRefreshToken, "login-refresh-token", "refreshed-access-token"} {
			require.NotContains(t, string(raw), secret)
		}
	}
}

func completeLogin(t *testing.T, issuer *mockIssuer, provider *Provider) *authpkg.ExternalIdentity {
	t.Helper()
	stateCookie, state := beginForCallback(t, provider, "/")
	issuer.nextNonce = state.Nonce
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"https://app.example.com/auth/sso/oidc/callback?code=good-code&state="+url.QueryEscape(state.State), nil)
	req.AddCookie(stateCookie)
	identity, _, err := provider.CompleteWithReturnTo(req)
	require.NoError(t, err)
	return identity
}

// TestLoginStoresTheRefreshTokenSealed drives a real code exchange against
// the mock IdP, the shared login tail and the catalog, and proves the stored
// column holds only ciphertext that opens to the token and refreshes.
func TestLoginStoresTheRefreshTokenSealed(t *testing.T) {
	const loginToken = "login-refresh-token-5d1b0c"
	issuer := newMockIssuer(t, "groups")
	issuer.loginRefreshToken = loginToken
	provider := newTestProvider(t, issuer, "groups")

	identity := completeLogin(t, issuer, provider)
	require.NotNil(t, identity.Refresh)
	require.Equal(t, ProviderName, identity.Refresh.Provider)
	require.Equal(t, issuer.URL(), identity.Refresh.Issuer)
	require.Equal(t, loginToken, identity.Refresh.Handle.Reveal())

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	users := authpkg.NewUserStore(db, authpkg.WithRefreshGrantSecret(testHashSecret))
	mapper, err := authpkg.NewRoleMapper("caesium-admins=admin", "")
	require.NoError(t, err)
	sso := authpkg.NewSSOService(users, authpkg.NewSessionStore(db), mapper, authpkg.WithSSOAuditLogger(authpkg.NewAuditLogger(db)))
	_, sess, err := sso.Complete(t.Context(), identity, ProviderName, "203.0.113.5", "agent")
	require.NoError(t, err)

	var raw []byte
	require.NoError(t, db.Raw("SELECT refresh_grant FROM users WHERE id = ?", sess.UserID).Row().Scan(&raw))
	require.NotEmpty(t, raw)
	require.False(t, bytes.Contains(raw, []byte(loginToken)), "the column must hold ciphertext only")
	var version int64
	require.NoError(t, db.Raw("SELECT refresh_grant_version FROM users WHERE id = ?", sess.UserID).Row().Scan(&version))
	require.Equal(t, int64(1), version)

	var auditRows []map[string]any
	require.NoError(t, db.Raw("SELECT * FROM audit_logs").Scan(&auditRows).Error)
	require.NotEmpty(t, auditRows)
	for _, row := range auditRows {
		encoded, err := json.Marshal(row)
		require.NoError(t, err)
		require.NotContains(t, string(encoded), loginToken)
	}

	owner := authpkg.NewRefreshLeaseOwner("test")
	claimed, err := users.ClaimRefreshLease(t.Context(), sess.UserID, 1, owner, time.Now(), time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)
	req, err := users.LoadLeasedRefreshGrant(t.Context(), sess.UserID, 1, owner, time.Now())
	require.NoError(t, err)
	require.Equal(t, loginToken, req.Grant.Handle.Reveal())

	// The stored grant redeems against the same IdP.
	issuer.refreshRotateTo = rotatedRefreshToken
	result, err := provider.RefreshGroups(t.Context(), req)
	require.NoError(t, err)
	require.True(t, result.GroupsKnown)
	require.ElementsMatch(t, []string{"data-eng", "ops"}, result.Groups)
	require.Equal(t, loginToken, issuer.lastRefreshForm.Get("refresh_token"))
	rotated := req.Grant
	rotated.Handle = result.Handle
	applied, err := users.CompleteRefreshSuccess(t.Context(), sess.UserID, 1, owner, result.Groups, &rotated, time.Now())
	require.NoError(t, err)
	require.True(t, applied)
	require.NoError(t, db.Raw("SELECT refresh_grant FROM users WHERE id = ?", sess.UserID).Row().Scan(&raw))
	require.False(t, bytes.Contains(raw, []byte(rotatedRefreshToken)))
	requireNoTokenInLogs(t)
}

// TestJWKSFailureAfterARotatingRefreshKeepsTheRotatedToken is the review
// finding on #646: the IdP rotated T to T2, then the ID token could not be
// verified (its JWKS failed on a cold cache). The stored grant must become T2
// (T is dead), while the groups and their timestamp stay untouched.
func TestJWKSFailureAfterARotatingRefreshKeepsTheRotatedToken(t *testing.T) {
	const loginToken = "login-refresh-token-jwks"
	issuer := newMockIssuer(t, "groups")
	issuer.loginRefreshToken = loginToken
	identity := completeLogin(t, issuer, newTestProvider(t, issuer, "groups"))

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	users := authpkg.NewUserStore(db, authpkg.WithRefreshGrantSecret(testHashSecret))
	mapper, err := authpkg.NewRoleMapper("caesium-admins=admin", "")
	require.NoError(t, err)
	_, sess, err := authpkg.NewSSOService(users, authpkg.NewSessionStore(db), mapper).
		Complete(t.Context(), identity, ProviderName, "", "")
	require.NoError(t, err)
	var before struct {
		Groups            string
		GroupsRefreshedAt time.Time
	}
	require.NoError(t, db.Raw("SELECT groups, groups_refreshed_at FROM users WHERE id = ?", sess.UserID).Row().
		Scan(&before.Groups, &before.GroupsRefreshedAt))

	// Another node, with a cold key cache, refreshes while the JWKS is down.
	coldNode := newTestProvider(t, issuer, "groups")
	issuer.keysUnavailable.Store(true)
	issuer.refreshRotateTo = rotatedRefreshToken
	issuer.refreshClaims = func(c map[string]any) { c["groups"] = []string{"caesium-admins", "escalated"} }

	owner := authpkg.NewRefreshLeaseOwner("cold-node")
	claimed, err := users.ClaimRefreshLease(t.Context(), sess.UserID, 1, owner, time.Now(), time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)
	req, err := users.LoadLeasedRefreshGrant(t.Context(), sess.UserID, 1, owner, time.Now())
	require.NoError(t, err)
	result, err := coldNode.RefreshGroups(t.Context(), req)
	require.NoError(t, err)
	require.False(t, result.GroupsKnown)
	require.True(t, result.Rotated)
	rotated := req.Grant
	rotated.Handle = result.Handle
	applied, err := users.CompleteRefreshGroupsUnknown(t.Context(), sess.UserID, 1, owner, &rotated)
	require.NoError(t, err)
	require.True(t, applied)

	var after struct {
		Groups            string
		GroupsRefreshedAt time.Time
	}
	require.NoError(t, db.Raw("SELECT groups, groups_refreshed_at FROM users WHERE id = ?", sess.UserID).Row().
		Scan(&after.Groups, &after.GroupsRefreshedAt))
	require.JSONEq(t, before.Groups, after.Groups, "unverified groups are never applied")
	require.True(t, before.GroupsRefreshedAt.Equal(after.GroupsRefreshedAt))

	claimed, err = users.ClaimRefreshLease(t.Context(), sess.UserID, 2, owner+"-2", time.Now(), time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)
	req, err = users.LoadLeasedRefreshGrant(t.Context(), sess.UserID, 2, owner+"-2", time.Now())
	require.NoError(t, err)
	require.Equal(t, rotatedRefreshToken, req.Grant.Handle.Reveal(), "the live token T2 is stored, not the dead T")
	requireNoTokenInLogs(t)
}

func TestLoginWithoutARefreshTokenCarriesNoGrant(t *testing.T) {
	issuer := newMockIssuer(t, "groups")
	provider := newTestProvider(t, issuer, "groups")
	identity := completeLogin(t, issuer, provider)
	require.Nil(t, identity.Refresh)
}

func TestRefreshGroupsReturnsGroupsFromAVerifiedIDToken(t *testing.T) {
	issuer := newMockIssuer(t, "groups")
	provider := newTestProvider(t, issuer, "groups")

	result, err := provider.RefreshGroups(t.Context(), refreshRequestFor(issuer, storedRefreshToken))
	require.NoError(t, err)
	require.True(t, result.GroupsKnown)
	require.Equal(t, []string{"data-eng", "ops"}, result.Groups)
	require.False(t, result.Rotated)
	require.Equal(t, storedRefreshToken, result.Handle.Reveal(), "an IdP that does not rotate keeps the redeemed token")

	require.Equal(t, int32(1), issuer.refreshCalls.Load())
	require.Equal(t, "refresh_token", issuer.lastRefreshForm.Get("grant_type"))
	require.Equal(t, storedRefreshToken, issuer.lastRefreshForm.Get("refresh_token"))
	require.Empty(t, issuer.lastRefreshForm.Get("client_secret"), "client_secret_basic is the default")
	user, pass, ok := (&http.Request{Header: http.Header{"Authorization": {issuer.lastRefreshAuth}}}).BasicAuth()
	require.True(t, ok)
	require.Equal(t, "caesium", user)
	require.Equal(t, "client-secret", pass)
	require.Nil(t, issuer.lastTokenForm, "the refresh never goes through the code-exchange path")
}

func TestRefreshGroupsReturnsTheRotatedToken(t *testing.T) {
	issuer := newMockIssuer(t, "groups")
	issuer.refreshRotateTo = rotatedRefreshToken
	provider := newTestProvider(t, issuer, "groups")

	result, err := provider.RefreshGroups(t.Context(), refreshRequestFor(issuer, storedRefreshToken))
	require.NoError(t, err)
	require.True(t, result.Rotated)
	require.Equal(t, rotatedRefreshToken, result.Handle.Reveal())
	require.True(t, result.GroupsKnown)
}

func TestRefreshGroupsUsesClientSecretPostWhenBasicIsNotOffered(t *testing.T) {
	issuer := newMockIssuer(t, "groups")
	issuer.tokenAuthMethods = []string{"client_secret_post", "private_key_jwt"}
	provider := newTestProvider(t, issuer, "groups")

	_, err := provider.RefreshGroups(t.Context(), refreshRequestFor(issuer, storedRefreshToken))
	require.NoError(t, err)
	require.Equal(t, "caesium", issuer.lastRefreshForm.Get("client_id"))
	require.Equal(t, "client-secret", issuer.lastRefreshForm.Get("client_secret"))
	require.Empty(t, issuer.lastRefreshAuth)

	require.False(t, postOnlyClientAuth(nil))
	require.False(t, postOnlyClientAuth([]string{"client_secret_basic", "client_secret_post"}))
	require.False(t, postOnlyClientAuth([]string{"private_key_jwt"}))
}

func TestRefreshGroupsInvalidGrantIsDefinitive(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized} {
		issuer := newMockIssuer(t, "groups")
		issuer.onRefresh = func(w http.ResponseWriter, _ *http.Request) bool {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Token is not active ` + storedRefreshToken + `"}`))
			return true
		}
		provider := newTestProvider(t, issuer, "groups")
		_, err := provider.RefreshGroups(t.Context(), refreshRequestFor(issuer, storedRefreshToken))
		refreshErr := requireRefreshError(t, err, authpkg.RefreshDefinitive, "invalid_grant")
		require.NoError(t, refreshErr.Err, "an invalid_grant carries no cause that could quote the body")
	}
}

func TestRefreshGroupsUncertainOutcomes(t *testing.T) {
	respond := func(status int, body string) func(http.ResponseWriter, *http.Request) bool {
		return func(w http.ResponseWriter, _ *http.Request) bool {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return true
		}
	}
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the token request must not follow a redirect")
	}))
	t.Cleanup(elsewhere.Close)

	for _, tc := range []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request) bool
		timeout time.Duration
		reason  string
	}{
		{name: "503", handler: respond(http.StatusServiceUnavailable, `{"error":"temporarily_unavailable"}`), reason: "http_503"},
		{name: "500 echoing invalid_grant", handler: respond(http.StatusInternalServerError, `{"error":"invalid_grant"}`), reason: "http_500"},
		{name: "invalid_client", handler: respond(http.StatusUnauthorized, `{"error":"invalid_client"}`), reason: "oauth_invalid_client"},
		{name: "unknown oauth error", handler: respond(http.StatusBadRequest, `{"error":"`+storedRefreshToken+`"}`), reason: "oauth_error"},
		{name: "400 without a body", handler: respond(http.StatusBadRequest, ``), reason: "oauth_error_unspecified"},
		{name: "unparsable 200", handler: respond(http.StatusOK, `<html>`+storedRefreshToken), reason: "unparsable_response"},
		{name: "200 carrying an error", handler: respond(http.StatusOK, `{"error":"invalid_grant"}`), reason: "oauth_error"},
		{name: "200 without access_token", handler: respond(http.StatusOK, `{"refresh_token":"`+rotatedRefreshToken+`"}`), reason: "missing_access_token"},
		{name: "oversized", handler: respond(http.StatusOK, `{"access_token":"`+strings.Repeat("a", maxTokenResponseBytes)+`"}`), reason: "response_too_large"},
		{name: "redirect", handler: func(w http.ResponseWriter, r *http.Request) bool {
			http.Redirect(w, r, elsewhere.URL+"/token", http.StatusTemporaryRedirect)
			return true
		}, reason: "http_307"},
		{name: "timeout after the request was written", timeout: 300 * time.Millisecond, handler: func(_ http.ResponseWriter, r *http.Request) bool {
			<-r.Context().Done()
			return true
		}, reason: "no_response"},
		{name: "connection reset mid-request", handler: func(w http.ResponseWriter, _ *http.Request) bool {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return false
			}
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.SetLinger(0)
			}
			_ = conn.Close()
			return true
		}, reason: "no_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issuer := newMockIssuer(t, "groups")
			issuer.onRefresh = tc.handler
			provider := newTestProvider(t, issuer, "groups")
			ctx := t.Context()
			if tc.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.timeout)
				defer cancel()
			}
			result, err := provider.RefreshGroups(ctx, refreshRequestFor(issuer, storedRefreshToken))
			requireRefreshError(t, err, authpkg.RefreshUncertain, tc.reason)
			require.Zero(t, result.Groups)
			require.True(t, result.Handle.IsZero(), "a failed redemption returns no handle to store")
			require.Equal(t, int32(1), issuer.refreshCalls.Load(), "exactly one request reaches the IdP")
		})
	}
}

func TestRefreshGroupsTransientOutcomesSendNothing(t *testing.T) {
	issuer := newMockIssuer(t, "groups")
	provider := newTestProvider(t, issuer, "groups")

	t.Run("unreachable host", func(t *testing.T) {
		listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := listener.Addr().String()
		require.NoError(t, listener.Close())
		unreachable := *provider
		unreachable.oauth2Config.Endpoint.TokenURL = "http://" + addr + "/token"
		_, err = unreachable.RefreshGroups(t.Context(), refreshRequestFor(issuer, storedRefreshToken))
		requireRefreshError(t, err, authpkg.RefreshTransient, "not_sent")
	})
	t.Run("unresolvable host", func(t *testing.T) {
		unresolvable := *provider
		unresolvable.oauth2Config.Endpoint.TokenURL = "http://caesium-refresh-test.invalid/token"
		// Whether resolution fails or the deadline ends it, nothing is sent.
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		_, err := unresolvable.RefreshGroups(ctx, refreshRequestFor(issuer, storedRefreshToken))
		requireRefreshError(t, err, authpkg.RefreshTransient, "not_sent")
	})
	t.Run("cancelled before send", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := provider.RefreshGroups(ctx, refreshRequestFor(issuer, storedRefreshToken))
		requireRefreshError(t, err, authpkg.RefreshTransient, "context_done")
		_, err = provider.redeemRefreshToken(ctx, authpkg.NewRefreshHandle(storedRefreshToken))
		requireRefreshError(t, err, authpkg.RefreshTransient, "not_sent")
	})
	t.Run("grant for another provider", func(t *testing.T) {
		req := refreshRequestFor(issuer, storedRefreshToken)
		req.Grant.Provider = "ldap"
		_, err := provider.RefreshGroups(t.Context(), req)
		requireRefreshError(t, err, authpkg.RefreshTransient, "provider_mismatch")
	})
	t.Run("grant minted by another issuer", func(t *testing.T) {
		req := refreshRequestFor(issuer, storedRefreshToken)
		req.Grant.Issuer = "https://old-idp.example.com"
		_, err := provider.RefreshGroups(t.Context(), req)
		requireRefreshError(t, err, authpkg.RefreshTransient, "issuer_mismatch")
	})
	t.Run("user from another issuer", func(t *testing.T) {
		req := refreshRequestFor(issuer, storedRefreshToken)
		req.Issuer = "https://old-idp.example.com"
		_, err := provider.RefreshGroups(t.Context(), req)
		requireRefreshError(t, err, authpkg.RefreshTransient, "issuer_mismatch")
	})
	t.Run("empty grant", func(t *testing.T) {
		_, err := provider.RefreshGroups(t.Context(), refreshRequestFor(issuer, ""))
		requireRefreshError(t, err, authpkg.RefreshTransient, "empty_grant")
	})
	require.Zero(t, issuer.refreshCalls.Load(), "no transient case may reach the IdP")
	requireNoTokenInLogs(t)
}

func TestRefreshGroupsNeverAcceptsAnUntrustedIDToken(t *testing.T) {
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	// A token that cannot be verified contributes no groups, but the token
	// endpoint has already redeemed the grant: its rotated token is the only
	// live one and is kept (GroupsKnown=false).
	for _, tc := range []struct {
		name  string
		setup func(*mockIssuer)
	}{
		{name: "wrong signing key", setup: func(m *mockIssuer) { m.refreshSigningKey = otherKey }},
		{name: "wrong audience", setup: func(m *mockIssuer) {
			m.refreshClaims = func(c map[string]any) { c["aud"] = "another-client" }
		}},
		{name: "expired", setup: func(m *mockIssuer) {
			m.refreshClaims = func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }
		}},
		{name: "another issuer", setup: func(m *mockIssuer) {
			m.refreshClaims = func(c map[string]any) { c["iss"] = "https://evil.example.com" }
		}},
		{name: "authorized party mismatch", setup: func(m *mockIssuer) {
			m.refreshClaims = func(c map[string]any) { c["azp"] = "another-client" }
		}},
		{name: "JWKS unavailable on a cold cache", setup: func(m *mockIssuer) { m.keysUnavailable.Store(true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, rotate := range []bool{true, false} {
				issuer := newMockIssuer(t, "groups")
				if rotate {
					issuer.refreshRotateTo = rotatedRefreshToken
				}
				issuer.refreshClaims = func(c map[string]any) { c["groups"] = []string{"caesium-admins"} }
				tc.setup(issuer)
				provider := newTestProvider(t, issuer, "groups")
				result, err := provider.RefreshGroups(t.Context(), refreshRequestFor(issuer, storedRefreshToken))
				require.NoError(t, err)
				require.False(t, result.GroupsKnown, "unverified groups are never applied")
				require.Nil(t, result.Groups)
				require.Equal(t, rotate, result.Rotated)
				if rotate {
					require.Equal(t, rotatedRefreshToken, result.Handle.Reveal(), "the rotated token is the only live one")
				} else {
					require.Equal(t, storedRefreshToken, result.Handle.Reveal())
				}
			}
		})
	}

	// A VERIFIED token for another identity keeps nothing, not even its
	// rotated token.
	for _, tc := range []struct {
		name   string
		setup  func(*mockIssuer)
		reason string
	}{
		{name: "another subject", setup: func(m *mockIssuer) {
			m.refreshClaims = func(c map[string]any) { c["sub"] = "someone-else" }
		}, reason: "identity_mismatch"},
		{name: "another subject with a malformed groups claim", setup: func(m *mockIssuer) {
			// Pre-PR review E1-r1: the malformed-groups branch used to keep
			// the rotation before the identity was compared.
			m.refreshClaims = func(c map[string]any) {
				c["sub"] = "someone-else"
				c["groups"] = []any{1, 2}
			}
		}, reason: "identity_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issuer := newMockIssuer(t, "groups")
			issuer.refreshRotateTo = rotatedRefreshToken
			tc.setup(issuer)
			provider := newTestProvider(t, issuer, "groups")
			result, err := provider.RefreshGroups(t.Context(), refreshRequestFor(issuer, storedRefreshToken))
			requireRefreshError(t, err, authpkg.RefreshUncertain, tc.reason)
			require.Zero(t, result.Groups)
			require.False(t, result.GroupsKnown)
			require.True(t, result.Handle.IsZero(), "an untrusted response's rotated token is not kept")
		})
	}
}

func TestRefreshGroupsMalformedGroupsClaimKeepsTheRotation(t *testing.T) {
	issuer := newMockIssuer(t, "groups")
	issuer.refreshRotateTo = rotatedRefreshToken
	issuer.refreshClaims = func(c map[string]any) { c["groups"] = []any{1, 2} }
	provider := newTestProvider(t, issuer, "groups")
	result, err := provider.RefreshGroups(t.Context(), refreshRequestFor(issuer, storedRefreshToken))
	require.NoError(t, err)
	require.False(t, result.GroupsKnown)
	require.Equal(t, rotatedRefreshToken, result.Handle.Reveal())
}

func userinfoResponder(t *testing.T, body map[string]any, status int, sawToken *string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if sawToken != nil {
			*sawToken = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if status != http.StatusOK {
			http.Error(w, "unavailable "+storedRefreshToken, status)
			return
		}
		writeJSON(w, body)
	}
}

// TestRefreshGroupsValidatesTheUserInfoAnswer covers the pre-PR review finding
// on #646: go-oidc only checks a signed userinfo answer's signature, so this
// provider checks it as OpenID Connect Core §5.3.2 asks. An answer for
// another client, or a signed one without iss or aud, applies no group but
// keeps the rotated token.
func TestRefreshGroupsValidatesTheUserInfoAnswer(t *testing.T) {
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	signedAnswer := func(m *mockIssuer, key *rsa.PrivateKey, edit func(map[string]any)) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			claims := map[string]any{"iss": m.URL(), "sub": "subject-123", "aud": "caesium", "groups": []string{"from-userinfo"}}
			edit(claims)
			if key == nil {
				key = m.key
			}
			w.Header().Set("Content-Type", "application/jwt")
			_, _ = w.Write([]byte(signJWT(key, claims)))
		}
	}
	jsonAnswer := func(edit func(map[string]any)) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			claims := map[string]any{"sub": "subject-123", "groups": []string{"from-userinfo"}}
			edit(claims)
			writeJSON(w, claims)
		}
	}
	keep := func(map[string]any) {}
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a userinfo redirect must not be followed with the bearer token")
	}))
	t.Cleanup(elsewhere.Close)

	for _, tc := range []struct {
		name     string
		answer   func(*mockIssuer) http.HandlerFunc
		accepted bool
		reason   string // set when the outcome is uncertain
	}{
		{name: "signed for this client", accepted: true,
			answer: func(m *mockIssuer) http.HandlerFunc { return signedAnswer(m, nil, keep) }},
		{name: "signed with an audience list including this client", accepted: true,
			answer: func(m *mockIssuer) http.HandlerFunc {
				return signedAnswer(m, nil, func(c map[string]any) { c["aud"] = []string{"api", "caesium"} })
			}},
		{name: "signed for another client",
			answer: func(m *mockIssuer) http.HandlerFunc {
				return signedAnswer(m, nil, func(c map[string]any) { c["aud"] = "another-client" })
			}},
		{name: "signed without an audience",
			answer: func(m *mockIssuer) http.HandlerFunc {
				return signedAnswer(m, nil, func(c map[string]any) { delete(c, "aud") })
			}},
		{name: "signed without an issuer",
			answer: func(m *mockIssuer) http.HandlerFunc {
				return signedAnswer(m, nil, func(c map[string]any) { delete(c, "iss") })
			}},
		{name: "signed by the wrong key",
			answer: func(m *mockIssuer) http.HandlerFunc { return signedAnswer(m, otherKey, keep) }},
		{name: "signed and within its time bounds", accepted: true,
			answer: func(m *mockIssuer) http.HandlerFunc {
				return signedAnswer(m, nil, func(c map[string]any) {
					c["exp"] = time.Now().Add(time.Minute).Unix()
					c["nbf"] = time.Now().Add(time.Minute).Unix() // within the allowed skew
				})
			}},
		{name: "signed and expired",
			answer: func(m *mockIssuer) http.HandlerFunc {
				return signedAnswer(m, nil, func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() })
			}},
		{name: "signed and not yet valid",
			answer: func(m *mockIssuer) http.HandlerFunc {
				return signedAnswer(m, nil, func(c map[string]any) { c["nbf"] = time.Now().Add(time.Hour).Unix() })
			}},
		{name: "signed with a malformed exp",
			answer: func(m *mockIssuer) http.HandlerFunc {
				return signedAnswer(m, nil, func(c map[string]any) { c["exp"] = "tomorrow" })
			}},
		{name: "signed with a malformed nbf",
			answer: func(m *mockIssuer) http.HandlerFunc {
				return signedAnswer(m, nil, func(c map[string]any) { c["nbf"] = []int{1} })
			}},
		{name: "plain JSON that has expired",
			answer: func(*mockIssuer) http.HandlerFunc {
				return jsonAnswer(func(c map[string]any) { c["exp"] = float64(time.Now().Add(-time.Second).Unix()) + 0.5 })
			}},
		{name: "signed by another issuer", reason: "identity_mismatch",
			answer: func(m *mockIssuer) http.HandlerFunc {
				return signedAnswer(m, nil, func(c map[string]any) { c["iss"] = "https://evil.example.com" })
			}},
		{name: "plain JSON for another client",
			answer: func(*mockIssuer) http.HandlerFunc {
				return jsonAnswer(func(c map[string]any) { c["aud"] = []string{"another-client"} })
			}},
		{name: "plain JSON naming this client", accepted: true,
			answer: func(*mockIssuer) http.HandlerFunc {
				return jsonAnswer(func(c map[string]any) { c["aud"] = "caesium" })
			}},
		{name: "plain JSON without a subject",
			answer: func(*mockIssuer) http.HandlerFunc {
				return jsonAnswer(func(c map[string]any) { delete(c, "sub") })
			}},
		{name: "a redirect is not followed",
			answer: func(*mockIssuer) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, elsewhere.URL+"/userinfo", http.StatusTemporaryRedirect)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issuer := newMockIssuer(t, "groups")
			issuer.refreshOmitIDToken = true
			issuer.refreshRotateTo = rotatedRefreshToken
			issuer.userinfo = tc.answer(issuer)
			provider := newTestProvider(t, issuer, "groups")
			result, err := provider.RefreshGroups(t.Context(), refreshRequestFor(issuer, storedRefreshToken))
			switch {
			case tc.reason != "":
				requireRefreshError(t, err, authpkg.RefreshUncertain, tc.reason)
				require.True(t, result.Handle.IsZero())
			case tc.accepted:
				require.NoError(t, err)
				require.True(t, result.GroupsKnown)
				require.Equal(t, []string{"from-userinfo"}, result.Groups)
			default:
				require.NoError(t, err)
				require.False(t, result.GroupsKnown, "no group from an answer that is not for this client")
				require.Nil(t, result.Groups)
				require.Equal(t, rotatedRefreshToken, result.Handle.Reveal(), "the rotated token is still kept")
			}
		})
	}
}

// TestRefreshGroupsWithoutAnIDToken covers OpenID Connect Core §12.2: a
// successful refresh may omit the ID token, possibly while rotating. That is
// never a failure; groups then come from userinfo or stay unknown.
func TestRefreshGroupsWithoutAnIDToken(t *testing.T) {
	t.Run("userinfo supplies the groups", func(t *testing.T) {
		issuer := newMockIssuer(t, "groups")
		issuer.refreshOmitIDToken = true
		issuer.refreshRotateTo = rotatedRefreshToken
		var bearer string
		issuer.userinfo = userinfoResponder(t, map[string]any{"sub": "subject-123", "groups": []string{"from-userinfo"}}, http.StatusOK, &bearer)
		provider := newTestProvider(t, issuer, "groups")

		result, err := provider.RefreshGroups(t.Context(), refreshRequestFor(issuer, storedRefreshToken))
		require.NoError(t, err)
		require.True(t, result.GroupsKnown)
		require.Equal(t, []string{"from-userinfo"}, result.Groups)
		require.Equal(t, rotatedRefreshToken, result.Handle.Reveal())
		require.Equal(t, "refreshed-access-token", bearer)
	})
	t.Run("userinfo naming the user's issuer", func(t *testing.T) {
		issuer := newMockIssuer(t, "groups")
		issuer.refreshOmitIDToken = true
		issuer.userinfo = func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"iss": issuer.URL(), "sub": "subject-123", "groups": []string{"from-userinfo"}})
		}
		provider := newTestProvider(t, issuer, "groups")
		result, err := provider.RefreshGroups(t.Context(), refreshRequestFor(issuer, storedRefreshToken))
		require.NoError(t, err)
		require.True(t, result.GroupsKnown)
		require.Equal(t, []string{"from-userinfo"}, result.Groups)
	})
	for name, iss := range map[string]any{"another issuer": "https://evil.example.com", "a non-string issuer": 7} {
		t.Run("userinfo naming "+name, func(t *testing.T) {
			// Pre-PR review E1-r1: go-oidc does not check a userinfo iss.
			issuer := newMockIssuer(t, "groups")
			issuer.refreshOmitIDToken = true
			issuer.refreshRotateTo = rotatedRefreshToken
			issuer.userinfo = userinfoResponder(t, map[string]any{"iss": iss, "sub": "subject-123", "groups": []string{"admins"}}, http.StatusOK, nil)
			provider := newTestProvider(t, issuer, "groups")
			result, err := provider.RefreshGroups(t.Context(), refreshRequestFor(issuer, storedRefreshToken))
			requireRefreshError(t, err, authpkg.RefreshUncertain, "identity_mismatch")
			require.Zero(t, result.Groups)
			require.True(t, result.Handle.IsZero())
		})
	}
	t.Run("userinfo for another subject", func(t *testing.T) {
		issuer := newMockIssuer(t, "groups")
		issuer.refreshOmitIDToken = true
		issuer.refreshRotateTo = rotatedRefreshToken
		issuer.userinfo = userinfoResponder(t, map[string]any{"sub": "someone-else", "groups": []string{"admins"}}, http.StatusOK, nil)
		provider := newTestProvider(t, issuer, "groups")
		result, err := provider.RefreshGroups(t.Context(), refreshRequestFor(issuer, storedRefreshToken))
		requireRefreshError(t, err, authpkg.RefreshUncertain, "identity_mismatch")
		require.Zero(t, result.Groups)
	})
	for name, setup := range map[string]func(*mockIssuer){
		"no userinfo endpoint": func(*mockIssuer) {},
		"userinfo fails": func(m *mockIssuer) {
			m.userinfo = userinfoResponder(t, nil, http.StatusBadGateway, nil)
		},
		"userinfo without the groups claim": func(m *mockIssuer) {
			m.userinfo = userinfoResponder(t, map[string]any{"sub": "subject-123"}, http.StatusOK, nil)
		},
		"userinfo with a malformed groups claim": func(m *mockIssuer) {
			m.userinfo = userinfoResponder(t, map[string]any{"sub": "subject-123", "groups": 7}, http.StatusOK, nil)
		},
	} {
		t.Run(name+" keeps the rotation", func(t *testing.T) {
			issuer := newMockIssuer(t, "groups")
			issuer.refreshOmitIDToken = true
			issuer.refreshRotateTo = rotatedRefreshToken
			setup(issuer)
			provider := newTestProvider(t, issuer, "groups")
			result, err := provider.RefreshGroups(t.Context(), refreshRequestFor(issuer, storedRefreshToken))
			require.NoError(t, err)
			require.False(t, result.GroupsKnown)
			require.Nil(t, result.Groups)
			require.True(t, result.Rotated)
			require.Equal(t, rotatedRefreshToken, result.Handle.Reveal())
		})
	}
	requireNoTokenInLogs(t)
}
