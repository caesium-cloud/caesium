package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// mintAgentKey mints a scoped agent-session credential bound to incidentID and
// returns its plaintext.
func mintAgentKey(t *testing.T, svc *auth.Service, incidentID uuid.UUID, allowlist []string) string {
	t.Helper()
	resp, err := svc.MintAgentSessionKey(incidentID, allowlist, time.Hour)
	require.NoError(t, err)
	return resp.Plaintext
}

// TestAgentTokenReachesOwnIncidentAgentRoute proves an agent token is accepted on
// its own incident's agent route AND that the frozen allowlist is injected for
// the context handlers.
func TestAgentTokenReachesOwnIncidentAgentRoute(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)
	incidentID := uuid.New()
	key := mintAgentKey(t, svc, incidentID, []string{"alpha", "beta"})

	req := httptest.NewRequest(http.MethodGet, "/v1/agent/incidents/"+incidentID.String()+"/bundle", nil)
	req.Header.Set("Authorization", "Bearer "+key)

	var aliases []string
	rec, err := callMiddleware(
		t, svc, auditor, limiter, req,
		&echo.RouteInfo{Path: "/v1/agent/incidents/:id/bundle", Method: http.MethodGet},
		echo.PathValues{{Name: "id", Value: incidentID.String()}},
		func(c *echo.Context) error {
			aliases = authmw.GetAllowedJobAliases(c)
			return c.String(http.StatusOK, "ok")
		},
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []string{"alpha", "beta"}, aliases)
}

// TestAgentTokenDeniedOnOtherIncident proves the cross-incident boundary: a token
// minted for incident X is 403'd on incident Y's agent routes.
func TestAgentTokenDeniedOnOtherIncident(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)
	incidentX := uuid.New()
	incidentY := uuid.New()
	key := mintAgentKey(t, svc, incidentX, []string{"alpha"})

	req := httptest.NewRequest(http.MethodGet, "/v1/agent/incidents/"+incidentY.String()+"/bundle", nil)
	req.Header.Set("Authorization", "Bearer "+key)

	_, err := callMiddleware(
		t, svc, auditor, limiter, req,
		&echo.RouteInfo{Path: "/v1/agent/incidents/:id/bundle", Method: http.MethodGet},
		echo.PathValues{{Name: "id", Value: incidentY.String()}},
		nil,
	)
	require.Error(t, err)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, he.Code)
	require.Equal(t, authmw.AgentScopeDenyMessage, he.Message)
}

// TestAgentTokenDeniedOnActionsOfOtherIncident proves the boundary holds for the
// mutating action route too (not just reads).
func TestAgentTokenDeniedOnActionsOfOtherIncident(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)
	incidentX := uuid.New()
	incidentY := uuid.New()
	key := mintAgentKey(t, svc, incidentX, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/agent/incidents/"+incidentY.String()+"/actions", nil)
	req.Header.Set("Authorization", "Bearer "+key)

	_, err := callMiddleware(
		t, svc, auditor, limiter, req,
		&echo.RouteInfo{Path: "/v1/agent/incidents/:id/actions", Method: http.MethodPost},
		echo.PathValues{{Name: "id", Value: incidentY.String()}},
		nil,
	)
	require.Error(t, err)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, he.Code)
}

// TestAgentTokenDeniedOnNonAgentRoutes proves the cross-route boundary: an agent
// token can never act as a general principal. It is 403'd on ordinary job/stats
// routes and — critically — on the global lineage-impact route (the frozen
// snapshot in the bundle is the only lineage view an agent gets).
func TestAgentTokenDeniedOnNonAgentRoutes(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)
	incidentID := uuid.New()
	key := mintAgentKey(t, svc, incidentID, []string{"alpha"})

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"jobs list", http.MethodGet, "/v1/jobs"},
		{"stats", http.MethodGet, "/v1/stats"},
		{"lineage impact", http.MethodGet, "/v1/lineage/impact"},
		{"events", http.MethodGet, "/v1/events"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Authorization", "Bearer "+key)
			_, err := callMiddleware(
				t, svc, auditor, limiter, req,
				&echo.RouteInfo{Path: tc.path, Method: tc.method},
				nil, nil,
			)
			require.Error(t, err)
			he, ok := err.(*echo.HTTPError)
			require.True(t, ok)
			require.Equal(t, http.StatusForbidden, he.Code, "agent token must be 403'd off %s", tc.path)
			require.Equal(t, authmw.AgentScopeDenyMessage, he.Message)
		})
	}
}

// TestAgentTokenReachesContextRoute proves the wildcard context passthrough is
// reachable for the token's own incident.
func TestAgentTokenReachesContextRoute(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)
	incidentID := uuid.New()
	key := mintAgentKey(t, svc, incidentID, []string{"alpha"})

	req := httptest.NewRequest(http.MethodGet, "/v1/agent/incidents/"+incidentID.String()+"/context/history", nil)
	req.Header.Set("Authorization", "Bearer "+key)

	rec, err := callMiddleware(
		t, svc, auditor, limiter, req,
		&echo.RouteInfo{Path: "/v1/agent/incidents/:id/context/*", Method: http.MethodGet},
		echo.PathValues{{Name: "id", Value: incidentID.String()}, {Name: "*", Value: "history"}},
		nil,
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
}
