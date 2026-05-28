package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBeginBuildsAuthURLAndStateCookie(t *testing.T) {
	issuer := newMockIssuer(t, "roles")
	provider := newTestProvider(t, issuer, "roles")

	req := httptest.NewRequest(http.MethodGet, "https://app.example.com/auth/sso/oidc/login", nil)
	rec := httptest.NewRecorder()
	redirectURL, err := provider.Begin(rec, req, "https://app.example.com/jobs?limit=10#run-1")
	require.NoError(t, err)

	redirect, err := url.Parse(redirectURL)
	require.NoError(t, err)
	query := redirect.Query()
	assert.Equal(t, issuer.URL()+"/authorize", redirect.Scheme+"://"+redirect.Host+redirect.Path)
	assert.Equal(t, "code", query.Get("response_type"))
	assert.Equal(t, "caesium", query.Get("client_id"))
	assert.Equal(t, "https://app.example.com/auth/sso/oidc/callback", query.Get("redirect_uri"))
	assert.ElementsMatch(t, []string{"openid", "profile", "email", "groups"}, strings.Fields(query.Get("scope")))
	assert.NotEmpty(t, query.Get("state"))
	assert.NotEmpty(t, query.Get("nonce"))
	assert.NotEmpty(t, query.Get("code_challenge"))
	assert.Equal(t, "S256", query.Get("code_challenge_method"))

	cookie := requireCookie(t, rec.Result(), DefaultStateCookieName)
	assert.True(t, cookie.HttpOnly)
	assert.False(t, cookie.Secure)
	assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
	assert.Equal(t, "/auth/sso/oidc", cookie.Path)
	assert.Positive(t, cookie.MaxAge)

	state, err := provider.decodeStateCookie(cookie.Value)
	require.NoError(t, err)
	assert.Equal(t, query.Get("state"), state.State)
	assert.Equal(t, query.Get("nonce"), state.Nonce)
	assert.Equal(t, "/jobs?limit=10#run-1", state.ReturnTo)
	assert.NotEmpty(t, state.CodeVerifier)
}

func TestBeginRejectsCrossOriginReturnTo(t *testing.T) {
	issuer := newMockIssuer(t, "groups")
	provider := newTestProvider(t, issuer, "groups")

	rec := httptest.NewRecorder()
	_, err := provider.Begin(rec, httptest.NewRequest(http.MethodGet, "/", nil), "https://evil.example.com/jobs")
	require.ErrorIs(t, err, ErrInvalidReturnTo)
	assert.Empty(t, rec.Result().Cookies())
}

func TestCompleteWithReturnToValidatesCallback(t *testing.T) {
	issuer := newMockIssuer(t, "roles")
	provider := newTestProvider(t, issuer, "roles")

	stateCookie, state := beginForCallback(t, provider, "/runs/abc")
	issuer.nextNonce = state.Nonce

	req := httptest.NewRequest(
		http.MethodGet,
		"https://app.example.com/auth/sso/oidc/callback?code=good-code&state="+url.QueryEscape(state.State),
		nil,
	)
	req.AddCookie(stateCookie)

	identity, returnTo, err := provider.CompleteWithReturnTo(req)
	require.NoError(t, err)
	assert.Equal(t, "/runs/abc", returnTo)
	assert.Equal(t, issuer.URL(), identity.Issuer)
	assert.Equal(t, "subject-123", identity.Subject)
	assert.Equal(t, "ada@example.com", identity.Email)
	assert.Equal(t, "Ada Lovelace", identity.DisplayName)
	assert.ElementsMatch(t, []string{"caesium-admins", "data-eng"}, identity.Groups)

	require.NotNil(t, issuer.lastTokenForm)
	assert.Equal(t, "good-code", issuer.lastTokenForm.Get("code"))
	assert.Equal(t, state.CodeVerifier, issuer.lastTokenForm.Get("code_verifier"))
	assert.Equal(t, "https://app.example.com/auth/sso/oidc/callback", issuer.lastTokenForm.Get("redirect_uri"))
}

func TestCompleteRejectsStateMismatch(t *testing.T) {
	issuer := newMockIssuer(t, "groups")
	provider := newTestProvider(t, issuer, "groups")

	stateCookie, _ := beginForCallback(t, provider, "/")
	req := httptest.NewRequest(http.MethodGet, "https://app.example.com/auth/sso/oidc/callback?code=good-code&state=wrong", nil)
	req.AddCookie(stateCookie)

	_, _, err := provider.CompleteWithReturnTo(req)
	require.ErrorIs(t, err, ErrInvalidState)
	assert.Nil(t, issuer.lastTokenForm)
}

func TestCompleteRejectsInvalidStateCookieBeforeExchange(t *testing.T) {
	tests := []struct {
		name       string
		prepare    func(*Provider, *http.Cookie, loginState) *http.Cookie
		wantErr    error
		wantErrMsg string
	}{
		{
			name: "tampered cookie",
			prepare: func(_ *Provider, cookie *http.Cookie, _ loginState) *http.Cookie {
				return cloneCookieWithValue(cookie, tamperCookieValue(cookie.Value))
			},
			wantErr:    ErrInvalidState,
			wantErrMsg: "signature mismatch",
		},
		{
			name: "expired cookie",
			prepare: func(provider *Provider, cookie *http.Cookie, state loginState) *http.Cookie {
				provider.now = func() time.Time {
					return time.Unix(state.ExpiresAt, 0).Add(time.Second)
				}
				return cookie
			},
			wantErr:    ErrInvalidState,
			wantErrMsg: "expired state",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issuer := newMockIssuer(t, "groups")
			provider := newTestProvider(t, issuer, "groups")

			stateCookie, state := beginForCallback(t, provider, "/")
			stateCookie = tt.prepare(provider, stateCookie, state)
			req := httptest.NewRequest(
				http.MethodGet,
				"https://app.example.com/auth/sso/oidc/callback?code=good-code&state="+url.QueryEscape(state.State),
				nil,
			)
			req.AddCookie(stateCookie)

			_, _, err := provider.CompleteWithReturnTo(req)
			require.ErrorIs(t, err, tt.wantErr)
			assert.Contains(t, err.Error(), tt.wantErrMsg)
			assert.Nil(t, issuer.lastTokenForm)
		})
	}
}

func TestCompleteRejectsEarlyCallbackErrorsBeforeStateCookie(t *testing.T) {
	tests := []struct {
		name    string
		rawURL  string
		wantErr error
	}{
		{
			name:    "authorization error",
			rawURL:  "https://app.example.com/auth/sso/oidc/callback?error=access_denied&error_description=user+canceled",
			wantErr: ErrAuthorizationFailed,
		},
		{
			name:    "missing code",
			rawURL:  "https://app.example.com/auth/sso/oidc/callback?state=ignored",
			wantErr: ErrMissingCode,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issuer := newMockIssuer(t, "groups")
			provider := newTestProvider(t, issuer, "groups")

			req := httptest.NewRequest(http.MethodGet, tt.rawURL, nil)
			req.AddCookie(&http.Cookie{Name: DefaultStateCookieName, Value: "not-a-valid-state-cookie"})

			_, _, err := provider.CompleteWithReturnTo(req)
			require.ErrorIs(t, err, tt.wantErr)
			assert.Nil(t, issuer.lastTokenForm)
		})
	}
}

func TestCompleteRejectsNonceMismatch(t *testing.T) {
	issuer := newMockIssuer(t, "groups")
	provider := newTestProvider(t, issuer, "groups")

	stateCookie, state := beginForCallback(t, provider, "/")
	issuer.nextNonce = "different-nonce"
	req := httptest.NewRequest(
		http.MethodGet,
		"https://app.example.com/auth/sso/oidc/callback?code=good-code&state="+url.QueryEscape(state.State),
		nil,
	)
	req.AddCookie(stateCookie)

	_, _, err := provider.CompleteWithReturnTo(req)
	require.ErrorIs(t, err, ErrInvalidNonce)
}

func TestCompleteRejectsExpiredIDToken(t *testing.T) {
	issuer := newMockIssuer(t, "groups")
	provider := newTestProvider(t, issuer, "groups")

	stateCookie, state := beginForCallback(t, provider, "/")
	issuer.nextNonce = state.Nonce
	issuer.nextExpiry = time.Now().Add(-time.Hour)
	req := httptest.NewRequest(
		http.MethodGet,
		"https://app.example.com/auth/sso/oidc/callback?code=good-code&state="+url.QueryEscape(state.State),
		nil,
	)
	req.AddCookie(stateCookie)

	_, _, err := provider.CompleteWithReturnTo(req)
	require.ErrorIs(t, err, ErrInvalidIDToken)
}

func TestCompleteRejectsAudienceMismatch(t *testing.T) {
	issuer := newMockIssuer(t, "groups")
	provider := newTestProvider(t, issuer, "groups")

	stateCookie, state := beginForCallback(t, provider, "/")
	issuer.nextNonce = state.Nonce
	issuer.nextAudience = "different-client"
	req := httptest.NewRequest(
		http.MethodGet,
		"https://app.example.com/auth/sso/oidc/callback?code=good-code&state="+url.QueryEscape(state.State),
		nil,
	)
	req.AddCookie(stateCookie)

	_, _, err := provider.CompleteWithReturnTo(req)
	require.ErrorIs(t, err, ErrInvalidIDToken)
	require.NotNil(t, issuer.lastTokenForm)
}

func TestExtractGroupsClaim(t *testing.T) {
	groups, err := extractGroups(json.RawMessage(`"one"`))
	require.NoError(t, err)
	assert.Equal(t, []string{"one"}, groups)

	groups, err = extractGroups(json.RawMessage(`["one","two"]`))
	require.NoError(t, err)
	assert.Equal(t, []string{"one", "two"}, groups)

	_, err = extractGroups(json.RawMessage(`[1]`))
	require.ErrorIs(t, err, ErrInvalidGroupsClaim)
}

func beginForCallback(t *testing.T, provider *Provider, returnTo string) (*http.Cookie, loginState) {
	t.Helper()

	rec := httptest.NewRecorder()
	_, err := provider.Begin(rec, httptest.NewRequest(http.MethodGet, "https://app.example.com/auth/sso/oidc/login", nil), returnTo)
	require.NoError(t, err)

	cookie := requireCookie(t, rec.Result(), DefaultStateCookieName)
	state, err := provider.decodeStateCookie(cookie.Value)
	require.NoError(t, err)
	return cookie, state
}

func newTestProvider(t *testing.T, issuer *mockIssuer, groupsClaim string) *Provider {
	t.Helper()

	provider, err := New(context.Background(), Config{
		IssuerURL:     issuer.URL(),
		ClientID:      "caesium",
		ClientSecret:  "client-secret",
		PublicBaseURL: "https://app.example.com",
		GroupsClaim:   groupsClaim,
		CookieSecure:  false,
		CookieSecret:  []byte("state-signing-secret"),
		HTTPClient:    issuer.Client(),
	})
	require.NoError(t, err)
	return provider
}

func requireCookie(t *testing.T, res *http.Response, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range res.Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("cookie %q not found", name)
	return nil
}

func cloneCookieWithValue(cookie *http.Cookie, value string) *http.Cookie {
	clone := *cookie
	clone.Value = value
	return &clone
}

func tamperCookieValue(value string) string {
	if len(value) == 0 {
		return "tampered"
	}
	replacement := byte('A')
	if value[len(value)-1] == replacement {
		replacement = 'B'
	}
	return value[:len(value)-1] + string(replacement)
}

type mockIssuer struct {
	server        *httptest.Server
	key           *rsa.PrivateKey
	groupsClaim   string
	nextNonce     string
	nextAudience  string
	nextExpiry    time.Time
	lastTokenForm url.Values
}

func newMockIssuer(t *testing.T, groupsClaim string) *mockIssuer {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	issuer := &mockIssuer{key: key, groupsClaim: groupsClaim}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", issuer.discovery)
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/token", issuer.token)
	mux.HandleFunc("/keys", issuer.keys)
	issuer.server = httptest.NewServer(mux)
	t.Cleanup(issuer.server.Close)
	return issuer
}

func (m *mockIssuer) URL() string {
	return m.server.URL
}

func (m *mockIssuer) Client() *http.Client {
	return m.server.Client()
}

func (m *mockIssuer) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                                m.URL(),
		"authorization_endpoint":                m.URL() + "/authorize",
		"token_endpoint":                        m.URL() + "/token",
		"jwks_uri":                              m.URL() + "/keys",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid", "profile", "email", "groups"},
	})
}

func (m *mockIssuer) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m.lastTokenForm = cloneValues(r.PostForm)
	nonce := m.nextNonce
	if nonce == "" {
		nonce = "nonce"
	}
	audience := m.nextAudience
	if audience == "" {
		audience = "caesium"
	}
	expiresAt := m.nextExpiry
	if expiresAt.IsZero() {
		expiresAt = time.Now().Add(time.Hour)
	}
	idToken := m.signIDToken(map[string]any{
		"iss":         m.URL(),
		"sub":         "subject-123",
		"aud":         audience,
		"exp":         expiresAt.Unix(),
		"iat":         time.Now().Add(-time.Minute).Unix(),
		"nonce":       nonce,
		"email":       "ada@example.com",
		"name":        "Ada Lovelace",
		m.groupsClaim: []string{"caesium-admins", "data-eng"},
	})
	writeJSON(w, map[string]any{
		"access_token": "access-token",
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     idToken,
	})
}

func (m *mockIssuer) keys(w http.ResponseWriter, _ *http.Request) {
	publicKey := m.key.PublicKey
	writeJSON(w, map[string]any{
		"keys": []map[string]any{
			{
				"kty": "RSA",
				"use": "sig",
				"kid": "test-key",
				"alg": "RS256",
				"n":   base64.RawURLEncoding.EncodeToString(publicKey.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(publicKey.E)).Bytes()),
			},
		},
	})
}

func (m *mockIssuer) signIDToken(claims map[string]any) string {
	header := map[string]any{"alg": "RS256", "kid": "test-key", "typ": "JWT"}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		panic(err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		panic(err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, m.key, crypto.SHA256, digest[:])
	if err != nil {
		panic(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		panic(err)
	}
}

func cloneValues(values url.Values) url.Values {
	out := make(url.Values, len(values))
	for key, value := range values {
		out[key] = append([]string(nil), value...)
	}
	return out
}
