package middleware_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/labstack/echo/v5"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// TestAuthStaleGroupsIsAnExplainedUnauthorized: a user refused for stale
// groups (identity-and-access §11.1) gets a 401 whose body names the reason
// stale_groups, through its session cookie and its user-bound key alike, and
// the failure is counted and audited under that reason. A fresh user and a
// plain API key are untouched by the refresher.
func TestAuthStaleGroupsIsAnExplainedUnauthorized(t *testing.T) {
	f := newSessionFixture(t, mustRoleMapper(t, "viewers=viewer"), &models.User{
		Email:         "sal@example.com",
		EmailVerified: true,
		Groups:        datatypes.JSON(`["viewers"]`),
		Role:          models.RoleViewer,
	})
	now := time.Now().UTC()
	f.setUserColumn(t, "groups_refreshed_at", now)

	ctx, cancel := context.WithCancel(context.Background())
	freshness, err := auth.NewGroupRefreshService(ctx, f.db, auth.GroupRefreshConfig{
		Interval: 15 * time.Minute, MaxStaleness: 24 * time.Hour, Timeout: time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		cancel()
		freshness.Wait()
	})
	f.deps.Sessions = auth.NewSessionStore(f.db, auth.WithSessionGroupFreshness(freshness))
	f.deps.Service = auth.NewService(f.db, auth.WithPolicySource(mustRoleMapper(t, "viewers=viewer")), auth.WithGroupFreshness(freshness))
	key := mintUserKey(t, f)
	plain, err := f.deps.Service.CreateKey(&auth.CreateKeyRequest{Role: models.RoleViewer})
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, f.do(t, http.MethodGet, "/v1/jobs"), "a fresh session is admitted")
	require.Equal(t, http.StatusOK, doBearer(t, f.deps, key.Plaintext, http.MethodGet, "/v1/jobs").status)

	f.setUserColumn(t, "stale_refused_at", now)
	before := staleGroupsFailures(t)

	cookie := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/jobs", strings.NewReader(""))
	cookie.AddCookie(&http.Cookie{Name: "caesium_session", Value: f.token})
	bearer := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/jobs", strings.NewReader(""))
	bearer.Header.Set("Authorization", "Bearer "+key.Plaintext)
	for name, req := range map[string]*http.Request{"session": cookie, "user-bound key": bearer} {
		ran := false
		rec, err := callMiddlewareWithDeps(t, f.deps, req, &echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodGet}, nil, func(c *echo.Context) error {
			ran = true
			return c.String(http.StatusOK, "ok")
		})
		require.NoError(t, err, name)
		require.False(t, ran, name)
		require.Equal(t, http.StatusUnauthorized, rec.Code, name)
		var body map[string]string
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), name)
		require.Equal(t, "stale_groups", body["reason"], name)
		require.NotEmpty(t, body["message"], name)
	}
	require.Equal(t, before+2, staleGroupsFailures(t))

	entries, err := f.auditor.Query(&auth.AuditQueryRequest{Action: auth.ActionAuthDenied, Limit: 10})
	require.NoError(t, err)
	stale := 0
	for _, entry := range entries {
		meta := map[string]any{}
		require.NoError(t, json.Unmarshal(entry.Metadata, &meta))
		if meta["reason"] == "stale_groups" {
			stale++
		}
	}
	require.Equal(t, 2, stale)

	got := doBearer(t, f.deps, plain.Plaintext, http.MethodGet, "/v1/jobs")
	require.Equal(t, http.StatusOK, got.status, "a plain API key never reaches the refresher")
	require.Equal(t, auth.PrincipalFromKey(got.key), got.principal)
}

func staleGroupsFailures(t *testing.T) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, metrics.AuthFailuresTotal.WithLabelValues("stale_groups").Write(&m))
	return m.GetCounter().GetValue()
}
