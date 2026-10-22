package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// mintAgentKey mints a scoped agent-session credential bound to incidentID and
// returns its plaintext.
func mintAgentKey(t *testing.T, svc *auth.Service, incidentID uuid.UUID, allowlist []string) string {
	t.Helper()
	resp, err := svc.MintAgentSessionKey(incidentID, allowlist, time.Hour)
	require.NoError(t, err)
	return resp.Plaintext
}

// seedAgentIncident stores an incident in namespace and one job per alias in
// the same namespace, and returns the incident id.
func seedAgentIncident(t *testing.T, db *gorm.DB, namespace string, aliases ...string) uuid.UUID {
	t.Helper()
	now := time.Now().UTC()
	var first uuid.UUID
	for _, alias := range aliases {
		id := uuid.New()
		if first == uuid.Nil {
			first = id
		}
		require.NoError(t, db.Create(&models.Job{ID: id, Alias: alias, Namespace: namespace, TriggerID: uuid.New(), CreatedAt: now, UpdatedAt: now}).Error)
	}
	if first == uuid.Nil {
		first = uuid.New()
	}
	incidentID := uuid.New()
	require.NoError(t, db.Create(&models.Incident{ID: incidentID, Namespace: namespace, JobID: first, Class: "unknown",
		Status: models.IncidentStatus("open"), DedupeKey: incidentID.String(), OpenedAt: now, CreatedAt: now, UpdatedAt: now}).Error)
	return incidentID
}

// TestAgentTokenReachesOwnIncidentAgentRoute proves an agent token is accepted on
// its own incident's agent route AND that the frozen allowlist is injected for
// the context handlers.
func TestAgentTokenReachesOwnIncidentAgentRoute(t *testing.T) {
	db, svc, auditor, limiter, _ := setupAuth(t)
	incidentID := seedAgentIncident(t, db, "default", "alpha", "beta")
	key := mintAgentKey(t, svc, incidentID, []string{"alpha", "beta"})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/agent/incidents/"+incidentID.String()+"/bundle", nil)
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

// TestAgentTokenAllowlistDropsJobsThatMovedOut proves the serve-time re-check
// (identity-and-access A8): a frozen job that has since moved out of the
// incident's namespace, or no longer exists, is not injected; an incident that
// cannot be read is a 404, never an unfiltered allowlist.
func TestAgentTokenAllowlistDropsJobsThatMovedOut(t *testing.T) {
	db, svc, auditor, limiter, _ := setupAuth(t)
	incidentID := seedAgentIncident(t, db, "marketing", "alpha", "beta")
	require.NoError(t, db.Model(&models.Job{}).Where("alias = ?", "beta").Update("namespace", "finance").Error)
	key := mintAgentKey(t, svc, incidentID, []string{"alpha", "beta", "gone"})

	call := func(incident uuid.UUID, token string) (int, []string, error) {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/agent/incidents/"+incident.String()+"/bundle", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		var aliases []string
		rec, err := callMiddleware(
			t, svc, auditor, limiter, req,
			&echo.RouteInfo{Path: "/v1/agent/incidents/:id/bundle", Method: http.MethodGet},
			echo.PathValues{{Name: "id", Value: incident.String()}},
			func(c *echo.Context) error {
				aliases = authmw.GetAllowedJobAliases(c)
				return c.String(http.StatusOK, "ok")
			},
		)
		return rec.Code, aliases, err
	}
	status, aliases, err := call(incidentID, key)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []string{"alpha"}, aliases)

	missing := uuid.New()
	_, _, err = call(missing, mintAgentKey(t, svc, missing, []string{"alpha"}))
	require.ErrorIs(t, err, echo.ErrNotFound)
}

// TestAgentTokenDeniedOnOtherIncident proves the cross-incident boundary: a token
// minted for incident X is 403'd on incident Y's agent routes.
func TestAgentTokenDeniedOnOtherIncident(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)
	incidentX := uuid.New()
	incidentY := uuid.New()
	key := mintAgentKey(t, svc, incidentX, []string{"alpha"})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/agent/incidents/"+incidentY.String()+"/bundle", nil)
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

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/agent/incidents/"+incidentY.String()+"/actions", nil)
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
			req := httptest.NewRequestWithContext(context.Background(), tc.method, tc.path, nil)
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
	db, svc, auditor, limiter, _ := setupAuth(t)
	incidentID := seedAgentIncident(t, db, "default", "alpha")
	key := mintAgentKey(t, svc, incidentID, []string{"alpha"})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/agent/incidents/"+incidentID.String()+"/context/history", nil)
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
