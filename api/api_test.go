package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
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
		var body map[string]bool
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.True(t, body["enabled"])
	})

	t.Run("disabled", func(t *testing.T) {
		rec := performRequest(t, authStatus(env.Environment{AuthMode: "none"}), http.MethodGet, "/auth/status")

		require.Equal(t, http.StatusOK, rec.Code)
		var body map[string]bool
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.False(t, body["enabled"])
	})
}

func TestRegisterMetricsPublicWhenAuthDisabled(t *testing.T) {
	e := echo.New()
	registerMetrics(e, env.Environment{AuthMode: "none"}, nil, nil, nil)

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
	registerMetrics(e, env.Environment{AuthMode: "api-key"}, svc, auditor, limiter)

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
