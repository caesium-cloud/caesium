package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/runlife"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

func TestAuthStatusReflectsAuthMode(t *testing.T) {
	t.Run("enabled", func(t *testing.T) {
		rec := performRequest(t, authStatus(env.Environment{AuthMode: "api-key"}), http.MethodGet, "/auth/status")

		require.Equal(t, http.StatusOK, rec.Code)
		var body struct {
			Enabled bool                `json:"enabled"`
			Methods []map[string]string `json:"methods"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.True(t, body.Enabled)
		require.Contains(t, body.Methods, map[string]string{"type": "api-key"})
	})

	t.Run("disabled", func(t *testing.T) {
		rec := performRequest(t, authStatus(env.Environment{AuthMode: "none"}), http.MethodGet, "/auth/status")

		require.Equal(t, http.StatusOK, rec.Code)
		var body struct {
			Enabled bool                `json:"enabled"`
			Methods []map[string]string `json:"methods"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.False(t, body.Enabled)
		require.Empty(t, body.Methods)
	})
}

func TestAuthStatusListsSSOMethods(t *testing.T) {
	rec := performRequest(t, authStatus(env.Environment{
		AuthMode:        "api-key",
		AuthOIDCEnabled: true,
		AuthSAMLEnabled: true,
		AuthLDAPEnabled: true,
	}), http.MethodGet, "/auth/status")

	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Enabled bool                `json:"enabled"`
		Methods []map[string]string `json:"methods"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.True(t, body.Enabled)
	require.Contains(t, body.Methods, map[string]string{"type": "api-key"})
	require.Contains(t, body.Methods, map[string]string{
		"type":     "oidc",
		"id":       "oidc",
		"label":    "Sign in with OIDC",
		"loginUrl": "/auth/sso/oidc/login",
		"mode":     "redirect",
	})
	require.Contains(t, body.Methods, map[string]string{
		"type":     "saml",
		"id":       "saml",
		"label":    "Sign in with SAML",
		"loginUrl": "/auth/sso/saml/login",
		"mode":     "redirect",
	})
	require.Contains(t, body.Methods, map[string]string{
		"type":     "ldap",
		"id":       "ldap",
		"label":    "Sign in with LDAP",
		"loginUrl": "/auth/sso/ldap/login",
		"mode":     "credential",
	})
}

func TestRegisterMetricsPublicWhenAuthDisabled(t *testing.T) {
	e := echo.New()
	registerMetrics(e, env.Environment{AuthMode: "none"}, nil, nil, nil, nil)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "caesium_")
}

func TestRegisterMetricsProtectedWhenAuthEnabled(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	svc := iauth.NewService(db)
	auditor := iauth.NewAuditLogger(db)
	limiter := iauth.NewRateLimiter(5, time.Minute)

	resp, err := svc.CreateKey(&iauth.CreateKeyRequest{
		Role:      models.RoleViewer,
		CreatedBy: "seed",
	})
	require.NoError(t, err)

	e := echo.New()
	registerMetrics(e, env.Environment{AuthMode: "api-key"}, svc, auditor, limiter, nil)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	req = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+resp.Plaintext)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "caesium_")
}

func TestRegisterSSORoutesProtectsLogoutWithCSRF(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	svc := iauth.NewService(db)
	auditor := iauth.NewAuditLogger(db)
	limiter := iauth.NewRateLimiter(5, time.Minute)
	sessions := iauth.NewSessionStore(db)
	user := &models.User{
		ID:        uuid.New(),
		Issuer:    "oidc",
		Subject:   "sub-1",
		Email:     "viewer@example.com",
		Role:      models.RoleViewer,
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, db.Create(user).Error)
	token, sess, err := sessions.Create(t.Context(), iauth.CreateSessionRequest{UserID: user.ID})
	require.NoError(t, err)

	e := echo.New()
	registerSSORoutes(e, env.Environment{AuthSessionCookieName: "caesium_session"}, svc, auditor, limiter, sessions, nil, SSOProviders{})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: "caesium_session", Value: token})
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)

	req = httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: "caesium_session", Value: token})
	req.Header.Set("X-CSRF-Token", sess.CSRFToken)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)

	_, _, err = sessions.Validate(t.Context(), token)
	require.ErrorIs(t, err, iauth.ErrSessionRevoked)
}

func TestRegisterSSORoutesBearerLogoutSkipsCSRFAndKeepsCredentials(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	svc := iauth.NewService(db)
	auditor := iauth.NewAuditLogger(db)
	limiter := iauth.NewRateLimiter(5, time.Minute)
	sessions := iauth.NewSessionStore(db)
	user := &models.User{
		ID:        uuid.New(),
		Issuer:    "oidc",
		Subject:   "sub-1",
		Email:     "viewer@example.com",
		Role:      models.RoleViewer,
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, db.Create(user).Error)
	sessionToken, sess, err := sessions.Create(t.Context(), iauth.CreateSessionRequest{UserID: user.ID})
	require.NoError(t, err)
	apiKey, err := svc.CreateKey(&iauth.CreateKeyRequest{
		Role:      models.RoleViewer,
		CreatedBy: "seed",
	})
	require.NoError(t, err)

	e := echo.New()
	registerSSORoutes(e, env.Environment{AuthSessionCookieName: "caesium_session"}, svc, auditor, limiter, sessions, nil, SSOProviders{})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/auth/logout", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey.Plaintext)
	req.AddCookie(&http.Cookie{Name: "caesium_session", Value: sessionToken})
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Empty(t, rec.Result().Cookies())

	storedKey, err := svc.ValidateKey(apiKey.Plaintext)
	require.NoError(t, err)
	require.Equal(t, apiKey.Key.ID, storedKey.ID)
	require.Nil(t, storedKey.RevokedAt)

	storedSession, _, err := sessions.Validate(t.Context(), sessionToken)
	require.NoError(t, err)
	require.Equal(t, sess.ID, storedSession.ID)
	require.Nil(t, storedSession.RevokedAt)

	entries, err := auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionAuthLogout, Limit: 10})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, apiKey.Key.KeyPrefix, entries[0].Actor)
	require.Equal(t, "api_key", entries[0].ResourceType)
	require.Equal(t, apiKey.Key.ID.String(), entries[0].ResourceID)
	require.Equal(t, iauth.OutcomeSuccess, entries[0].Outcome)

	var metadata map[string]any
	require.NoError(t, json.Unmarshal(entries[0].Metadata, &metadata))
	require.Equal(t, true, metadata["noop"])
}

func TestRegisterSSORoutesWhoamiReturnsSessionCSRF(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	svc := iauth.NewService(db)
	auditor := iauth.NewAuditLogger(db)
	limiter := iauth.NewRateLimiter(5, time.Minute)
	sessions := iauth.NewSessionStore(db)
	user := &models.User{
		ID:        uuid.New(),
		Issuer:    "oidc",
		Subject:   "sub-1",
		Email:     "viewer@example.com",
		Role:      models.RoleViewer,
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, db.Create(user).Error)
	token, sess, err := sessions.Create(t.Context(), iauth.CreateSessionRequest{UserID: user.ID})
	require.NoError(t, err)

	e := echo.New()
	registerSSORoutes(e, env.Environment{AuthSessionCookieName: "caesium_session"}, svc, auditor, limiter, sessions, nil, SSOProviders{})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/auth/whoami", nil)
	req.AddCookie(&http.Cookie{Name: "caesium_session", Value: token})
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, string(iauth.PrincipalUser), body["kind"])
	require.Equal(t, "viewer@example.com", body["subject"])
	require.Equal(t, string(models.RoleViewer), body["role"])
	require.Equal(t, sess.CSRFToken, body["csrf_token"])
}

func TestRegisterSSORoutesMountsOIDCRedirectProviderRoutes(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	e := echo.New()
	registerSSORoutes(
		e,
		env.Environment{AuthSessionCookieName: "caesium_session", AuthOIDCEnabled: true},
		nil,
		nil,
		nil,
		iauth.NewSessionStore(db),
		nil,
		SSOProviders{OIDC: noopRedirectAuthenticator{}},
	)

	require.True(t, hasRoute(e, http.MethodGet, "/auth/sso/oidc/login"))
	require.True(t, hasRoute(e, http.MethodGet, "/auth/sso/oidc/callback"))
}

func TestRegisterSSORoutesMountsSAMLRedirectProviderRoutes(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	e := echo.New()
	registerSSORoutes(
		e,
		env.Environment{AuthSessionCookieName: "caesium_session", AuthSAMLEnabled: true},
		nil,
		nil,
		nil,
		iauth.NewSessionStore(db),
		nil,
		SSOProviders{SAML: noopRedirectAuthenticator{}},
	)

	require.True(t, hasRoute(e, http.MethodGet, "/auth/sso/saml/login"))
	require.True(t, hasRoute(e, http.MethodPost, "/auth/sso/saml/acs"))
	require.True(t, hasRoute(e, http.MethodGet, "/auth/sso/saml/metadata"))
}

func TestRegisterSSORoutesSkipsSAMLRedirectProviderRoutesWithoutProvider(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	e := echo.New()
	registerSSORoutes(
		e,
		env.Environment{AuthSessionCookieName: "caesium_session", AuthSAMLEnabled: true},
		nil,
		nil,
		nil,
		iauth.NewSessionStore(db),
		nil,
		SSOProviders{},
	)

	require.False(t, hasRoute(e, http.MethodGet, "/auth/sso/saml/login"))
	require.False(t, hasRoute(e, http.MethodPost, "/auth/sso/saml/acs"))
	require.False(t, hasRoute(e, http.MethodGet, "/auth/sso/saml/metadata"))
}

func TestRegisterSSORoutesMountsLDAPCredentialProviderRoute(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	e := echo.New()
	registerSSORoutes(
		e,
		env.Environment{AuthSessionCookieName: "caesium_session", AuthLDAPEnabled: true},
		nil,
		nil,
		nil,
		iauth.NewSessionStore(db),
		nil,
		SSOProviders{LDAP: noopCredentialAuthenticator{}},
	)

	require.True(t, hasRoute(e, http.MethodPost, "/auth/sso/ldap/login"))
}

func TestRegisterSSORoutesRateLimitsLDAPCredentialFailures(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	e := echo.New()
	registerSSORoutes(
		e,
		env.Environment{AuthSessionCookieName: "caesium_session", AuthLDAPEnabled: true},
		nil,
		nil,
		iauth.NewRateLimiter(1, time.Minute),
		iauth.NewSessionStore(db),
		iauth.NewSSOService(nil, nil, nil),
		SSOProviders{LDAP: noopCredentialAuthenticator{}},
	)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/auth/sso/ldap/login", strings.NewReader(`{"username":"ada","password":"bad"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	req.RemoteAddr = "198.51.100.10:1234"
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	req = httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/auth/sso/ldap/login", strings.NewReader(`{"username":"ada","password":"bad"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	req.RemoteAddr = "198.51.100.10:1234"
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.NotEmpty(t, rec.Header().Get("Retry-After"))
}

func TestCredentialLoginFailedClassifiesOnlyAuthFailures(t *testing.T) {
	require.True(t, credentialLoginFailed(echo.NewHTTPError(http.StatusUnauthorized, "invalid"), 0))
	require.True(t, credentialLoginFailed(fmt.Errorf("wrapped: %w", echo.NewHTTPError(http.StatusForbidden, "denied")), 0))
	require.False(t, credentialLoginFailed(echo.NewHTTPError(http.StatusInternalServerError, "failed"), 0))
	require.False(t, credentialLoginFailed(fmt.Errorf("raw server error"), 0))
	require.True(t, credentialLoginFailed(nil, http.StatusUnauthorized))
	require.True(t, credentialLoginFailed(nil, http.StatusForbidden))
	require.False(t, credentialLoginFailed(nil, http.StatusInternalServerError))
}

func TestRegisterSSORoutesSkipsLDAPCredentialProviderRouteWithoutProvider(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	e := echo.New()
	registerSSORoutes(
		e,
		env.Environment{AuthSessionCookieName: "caesium_session", AuthLDAPEnabled: true},
		nil,
		nil,
		nil,
		iauth.NewSessionStore(db),
		nil,
		SSOProviders{},
	)

	require.False(t, hasRoute(e, http.MethodPost, "/auth/sso/ldap/login"))
}

func TestRegisterInternalWakeupRequiresToken(t *testing.T) {
	e := echo.New()
	var called atomic.Bool
	var gotID string
	var gotTTL int
	registerInternalWakeup(e, env.Environment{InternalWakeupToken: "secret"}, func(_ context.Context, id string, ttl int) {
		called.Store(true)
		gotID = id
		gotTTL = ttl
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/internal/wakeup", strings.NewReader(`{"id":"abc","ttl":2}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.False(t, called.Load())

	req = httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/internal/wakeup", strings.NewReader(`{"id":"abc","ttl":2}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.True(t, called.Load())
	require.Equal(t, "abc", gotID)
	require.Equal(t, 2, gotTTL)
}

func TestRegisterInternalWakeupSkippedWhenDisabled(t *testing.T) {
	e := echo.New()
	registerInternalWakeup(e, env.Environment{}, func(context.Context, string, int) {})

	require.False(t, hasRoute(e, http.MethodPost, "/internal/wakeup"))
}

func performRequest(t *testing.T, handler echo.HandlerFunc, method, path string) *httptest.ResponseRecorder {
	t.Helper()

	e := echo.New()
	req := httptest.NewRequestWithContext(context.Background(), method, path, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler(c)
	require.NoError(t, err)
	return rec
}

func hasRoute(e *echo.Echo, method, path string) bool {
	for _, route := range e.Router().Routes() {
		if route.Method == method && route.Path == path {
			return true
		}
	}
	return false
}

type noopRedirectAuthenticator struct{}

func (noopRedirectAuthenticator) Name() string {
	return "oidc"
}

func (noopRedirectAuthenticator) Begin(http.ResponseWriter, *http.Request, string) (string, error) {
	return "https://idp.example/authorize", nil
}

func (noopRedirectAuthenticator) Complete(*http.Request) (*iauth.ExternalIdentity, error) {
	return nil, nil
}

type noopCredentialAuthenticator struct{}

func (noopCredentialAuthenticator) Name() string {
	return "ldap"
}

func (noopCredentialAuthenticator) Authenticate(context.Context, string, string) (*iauth.ExternalIdentity, error) {
	return nil, nil
}

func prepareAPILifetimeTest(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { require.NoError(t, env.Process()) })
	t.Setenv("CAESIUM_PORT", "0")
	t.Setenv("CAESIUM_AUTH_MODE", "none")
	t.Setenv("CAESIUM_SHUTDOWN_GRACE_PERIOD", "20ms")
	require.NoError(t, env.Process())
	oldServe := serveAPI
	t.Cleanup(func() { serveAPI = oldServe })
}

func TestStandaloneStartShutdownJoinsOwnerAfterServeReturns(t *testing.T) {
	prepareAPILifetimeTest(t)
	ready := make(chan *http.Server, 1)
	serveAPI = func(srv *http.Server, ln net.Listener) error { ready <- srv; return srv.Serve(ln) }
	serving := make(chan error, 1)
	go func() { serving <- Start(context.Background(), nil, nil, nil, nil, nil, nil, SSOProviders{}, nil) }()
	srv := <-ready
	requestBase := srv.BaseContext(nil)
	owner := runlife.FromContext(requestBase)
	require.NotNil(t, owner)
	child, release, err := owner.Reserve(requestBase)
	require.NoError(t, err)
	shutting := make(chan error, 1)
	go func() { shutting <- Shutdown(context.Background()) }()
	select {
	case <-child.Done():
	case <-time.After(time.Second):
		t.Fatal("owned work not cancelled")
	}
	require.NoError(t, <-serving)
	select {
	case err := <-shutting:
		t.Fatalf("shutdown skipped owned work: %v", err)
	default:
	}
	release()
	require.NoError(t, <-shutting)
	apiServer.Lock()
	remaining := apiServer.owner
	apiServer.Unlock()
	require.Nil(t, remaining)
}

func TestSharedOwnerSurvivesAPIHTTPDrain(t *testing.T) {
	prepareAPILifetimeTest(t)
	owner := runlife.New(context.Background())
	defer owner.CloseAndCancel()
	ready := make(chan *http.Server, 1)
	serveAPI = func(srv *http.Server, ln net.Listener) error { ready <- srv; return srv.Serve(ln) }
	root, cancel := context.WithCancel(context.Background())
	ctx := runlife.WithSupervisor(root, owner)
	serving := make(chan error, 1)
	go func() { serving <- Start(ctx, nil, nil, nil, nil, nil, nil, SSOProviders{}, nil) }()
	srv := <-ready
	cancel()
	require.NoError(t, srv.BaseContext(nil).Err(), "HTTP request drain is independent of process cancellation")
	require.NoError(t, Shutdown(context.Background()))
	require.NoError(t, <-serving)
	_, release, err := owner.Reserve(srv.BaseContext(nil))
	require.NoError(t, err, "shared owner closes only in coordinator")
	release()
}

func TestUnexpectedServeExitRetainsOwnerAfterJoinTimeout(t *testing.T) {
	prepareAPILifetimeTest(t)
	fault := errors.New("serve failed")
	var owner *runlife.Supervisor
	var release func()
	serveAPI = func(srv *http.Server, _ net.Listener) error {
		owner = runlife.FromContext(srv.BaseContext(nil))
		_, release, _ = owner.Reserve(srv.BaseContext(nil))
		return fault
	}
	err := Start(context.Background(), nil, nil, nil, nil, nil, nil, SSOProviders{}, nil)
	require.ErrorIs(t, err, fault)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	apiServer.Lock()
	retained, srv := apiServer.owner, apiServer.srv
	apiServer.Unlock()
	require.Same(t, owner, retained)
	require.Nil(t, srv)
	_, _, err = owner.Reserve(context.Background())
	require.ErrorIs(t, err, runlife.ErrClosed)
	release()
	require.NoError(t, Shutdown(context.Background()))
}
