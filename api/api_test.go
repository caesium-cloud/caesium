package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

func TestRegisterGraphQLWhenAuthDisabled(t *testing.T) {
	e := echo.New()
	registerGraphQL(e, env.Environment{AuthMode: "none"})

	require.True(t, hasRoute(e, http.MethodGet, "/gql"))
}

func TestRegisterGraphQLSkippedWhenAuthEnabled(t *testing.T) {
	e := echo.New()
	registerGraphQL(e, env.Environment{AuthMode: "api-key"})

	require.False(t, hasRoute(e, http.MethodGet, "/gql"))
}

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
	require.Contains(t, body.Methods, map[string]string{"type": "oidc", "loginUrl": "/auth/sso/oidc/login"})
	require.Contains(t, body.Methods, map[string]string{"type": "saml", "loginUrl": "/auth/sso/saml/login"})
	require.Contains(t, body.Methods, map[string]string{"type": "ldap"})
}

func TestRegisterMetricsPublicWhenAuthDisabled(t *testing.T) {
	e := echo.New()
	registerMetrics(e, env.Environment{AuthMode: "none"}, nil, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
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
	registerSSORoutes(e, env.Environment{AuthSessionCookieName: "caesium_session"}, svc, auditor, limiter, sessions, nil)

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: "caesium_session", Value: token})
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)

	req = httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: "caesium_session", Value: token})
	req.Header.Set("X-CSRF-Token", sess.CSRFToken)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)

	_, _, err = sessions.Validate(t.Context(), token)
	require.ErrorIs(t, err, iauth.ErrSessionRevoked)
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

	req := httptest.NewRequest(http.MethodPost, "/internal/wakeup", strings.NewReader(`{"id":"abc","ttl":2}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.False(t, called.Load())

	req = httptest.NewRequest(http.MethodPost, "/internal/wakeup", strings.NewReader(`{"id":"abc","ttl":2}`))
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
	req := httptest.NewRequest(method, path, nil)
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
