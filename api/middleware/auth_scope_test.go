package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// whoamiRoute is the identity route as the RBAC normaliser sees it. Note it is
// mounted at /auth/whoami (api/api.go), NOT under the /v1 group.
var whoamiRoute = &echo.RouteInfo{Path: "/auth/whoami", Method: http.MethodGet}

// TestMiddlewareWhoamiAllowsScopedAPIKey pins the C1 decision: whoami is
// identity, not resource access, so a job-scoped key resolves its own principal.
// Before the /auth/whoami case in authorizeScope, a scoped key fell through to
// the trailing "insufficient permissions" deny and got 403 — which also made
// scoped keys unable to complete the UI's api-key login.
func TestMiddlewareWhoamiAllowsScopedAPIKey(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)
	key := createKey(t, svc, models.RoleViewer, &models.KeyScope{Jobs: []string{"alpha"}})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/auth/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+key)

	var principal *auth.Principal
	rec, err := callMiddleware(t, svc, auditor, limiter, req, whoamiRoute, nil, func(c *echo.Context) error {
		principal = authmw.GetPrincipal(c)
		return c.String(http.StatusOK, "ok")
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, principal)
	require.Equal(t, auth.PrincipalAPIKey, principal.Kind)
	require.Equal(t, models.RoleViewer, principal.Role)

	// The allow grants identity only: no job aliases are injected, so the
	// handler cannot infer any resource access from having passed the gate.
	jobs, err := auth.ScopeJobs(principal.Scope)
	require.NoError(t, err)
	require.Equal(t, []string{"alpha"}, jobs)
}

// TestMiddlewareWhoamiAllowsUnscopedAPIKey guards the unchanged half: an
// unscoped key still reaches whoami (it always did, via the len==0 early
// return) and the new case must not have moved that.
func TestMiddlewareWhoamiAllowsUnscopedAPIKey(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)
	key := createKey(t, svc, models.RoleViewer, nil)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/auth/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+key)

	rec, err := callMiddleware(t, svc, auditor, limiter, req, whoamiRoute, nil, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
}

// TestMiddlewareWhoamiDeniesAgentSessionToken is the security half of C1: the
// whoami allow is for API-key principals ONLY. An agent-session token stays
// confined to its incident's /v1/agent/* surface, so it must still be 403'd
// here — an agent must never be able to complete a UI login.
func TestMiddlewareWhoamiDeniesAgentSessionToken(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)
	key := mintAgentKey(t, svc, uuid.New(), []string{"alpha"})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/auth/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+key)

	called := false
	_, err := callMiddleware(t, svc, auditor, limiter, req, whoamiRoute, nil, func(c *echo.Context) error {
		called = true
		return c.String(http.StatusOK, "ok")
	})
	require.Error(t, err)
	require.False(t, called)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, he.Code)
	require.Equal(t, authmw.AgentScopeDenyMessage, he.Message)
}

// TestMiddlewareScopedKeyStillDeniedOnUnrelatedGlobalRoute proves the whoami
// case widened exactly one path and nothing else: a scoped key remains denied
// on a global route that names no job (the switch falls through to the trailing
// deny). Without this, a `default:`-shaped mistake in the switch would go unseen.
func TestMiddlewareScopedKeyStillDeniedOnUnrelatedGlobalRoute(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)
	key := createKey(t, svc, models.RoleViewer, &models.KeyScope{Jobs: []string{"alpha"}})

	for _, route := range []*echo.RouteInfo{
		{Path: "/v1/stats", Method: http.MethodGet},
		{Path: "/v1/system/nodes", Method: http.MethodGet},
		{Path: "/auth/logout", Method: http.MethodPost},
	} {
		req := httptest.NewRequestWithContext(context.Background(), route.Method, route.Path, nil)
		req.Header.Set("Authorization", "Bearer "+key)

		_, err := callMiddleware(t, svc, auditor, limiter, req, route, nil, nil)
		require.Errorf(t, err, "%s %s must stay denied for a scoped key", route.Method, route.Path)

		he, ok := err.(*echo.HTTPError)
		require.True(t, ok)
		require.Equal(t, http.StatusForbidden, he.Code)
	}
}
