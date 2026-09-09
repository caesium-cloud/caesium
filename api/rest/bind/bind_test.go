package bind

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

func TestAllProtectsRESTButLeavesWebhooksPublic(t *testing.T) {
	t.Setenv("CAESIUM_AUTH_MODE", "api-key")
	t.Setenv("CAESIUM_DATABASE_PATH", t.TempDir())
	require.NoError(t, env.Process())

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	svc := auth.NewService(db)
	auditor := auth.NewAuditLogger(db)
	limiter := auth.NewRateLimiter(10, time.Minute)

	originalFactory := webhookHandlerFactory
	t.Cleanup(func() {
		webhookHandlerFactory = originalFactory
	})

	webhookCalls := 0
	webhookHandlerFactory = func(_ *auth.AuditLogger) func(*echo.Context) error {
		return func(c *echo.Context) error {
			webhookCalls++
			return c.String(http.StatusAccepted, "webhook")
		}
	}

	e := echo.New()
	All(e.Group("/v1"), nil, svc, auditor, limiter, nil)

	protectedReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/jobs", nil)
	protectedRec := httptest.NewRecorder()
	e.ServeHTTP(protectedRec, protectedReq)
	require.Equal(t, http.StatusUnauthorized, protectedRec.Code)

	webhookReq := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/hooks/github/push", strings.NewReader(`{}`))
	webhookRec := httptest.NewRecorder()
	e.ServeHTTP(webhookRec, webhookReq)
	require.Equal(t, http.StatusAccepted, webhookRec.Code)
	require.Equal(t, 1, webhookCalls)
}

func TestProtectedGatesContractGraphRoute(t *testing.T) {
	t.Setenv("CAESIUM_CONTRACT_ENFORCEMENT", "")
	off := echo.New()
	Protected(off.Group("/v1"), nil, nil)
	require.False(t, hasRoute(off, http.MethodGet, "/v1/contracts/graph"))

	t.Setenv("CAESIUM_CONTRACT_ENFORCEMENT", "fail")
	on := echo.New()
	Protected(on.Group("/v1"), nil, nil)
	require.True(t, hasRoute(on, http.MethodGet, "/v1/contracts/graph"))
}

// TestProtectedGatesDatasetAssertionRoutes pins arc convention 1 for the data
// circuit breaker: off means NO ROUTE, not a route that 404s.
//
// It has to be a unit test. H-1 turned CAESIUM_DATA_ASSERTIONS_ENABLED on for
// every self-server lane, so no integration lane can ever observe the off
// state — an integration test would only ever prove the on half.
func TestProtectedGatesDatasetAssertionRoutes(t *testing.T) {
	t.Setenv("CAESIUM_DATABASE_PATH", t.TempDir())

	t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "false")
	require.NoError(t, env.Process())
	off := echo.New()
	Protected(off.Group("/v1"), nil, nil)
	require.False(t, hasRoute(off, http.MethodPost, "/v1/datasets/holds/:id/release"))
	require.False(t, hasRoute(off, http.MethodGet, "/v1/datasets/holds"))
	require.False(t, hasRoute(off, http.MethodGet, "/v1/datasets/:ns/:name/metrics"))
	// The base freshness dataset surface is unconditional and must stay so.
	require.True(t, hasRoute(off, http.MethodGet, "/v1/datasets"))

	t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "true")
	require.NoError(t, env.Process())
	t.Cleanup(func() {
		t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "false")
		require.NoError(t, env.Process())
	})
	on := echo.New()
	Protected(on.Group("/v1"), nil, nil)
	require.True(t, hasRoute(on, http.MethodPost, "/v1/datasets/holds/:id/release"))
	require.True(t, hasRoute(on, http.MethodGet, "/v1/datasets/holds"))
	require.True(t, hasRoute(on, http.MethodGet, "/v1/datasets/:ns/:name/metrics"))
}

// TestProtectedRoutesAllHaveAnRBACEntry closes the gap that let the hold-release
// route ship 403-ing every caller on the auth lane.
//
// The auth middleware denies any route missing from internal/auth's policy table
// as "unknown_route" — a 403 indistinguishable from a role failure — and NO test
// that runs with auth off can see it, because with auth off the middleware is
// never attached. Every route this binder mounts must therefore be listed.
func TestProtectedRoutesAllHaveAnRBACEntry(t *testing.T) {
	t.Setenv("CAESIUM_DATABASE_PATH", t.TempDir())
	t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "true")
	t.Setenv("CAESIUM_CONTRACT_ENFORCEMENT", "fail")
	t.Setenv("CAESIUM_AGENT_REMEDIATION_ENABLED", "true")
	t.Setenv("CAESIUM_AUTH_MODE", "api-key")
	t.Setenv("CAESIUM_LOG_CONSOLE_ENABLED", "true")
	t.Setenv("CAESIUM_DATABASE_CONSOLE_ENABLED", "true")
	require.NoError(t, env.Process())
	t.Cleanup(func() {
		t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "false")
		require.NoError(t, env.Process())
	})

	e := echo.New()
	Protected(e.Group("/v1"), nil, nil)

	for _, route := range e.Router().Routes() {
		path := authmw.NormalizeRoutePath(route.Path)
		if _, ok := auth.RequiredRole(route.Method, path); !ok {
			t.Errorf("route %s %s (normalised %s) has no internal/auth RBAC entry; "+
				"the middleware will deny it as unknown_route (403) under CAESIUM_AUTH_MODE=api-key",
				route.Method, route.Path, path)
		}
	}
}

func hasRoute(e *echo.Echo, method, path string) bool {
	for _, route := range e.Router().Routes() {
		if route.Method == method && route.Path == path {
			return true
		}
	}
	return false
}
