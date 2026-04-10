package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupAuth(t *testing.T) (*gorm.DB, *auth.Service, *auth.AuditLogger, *auth.RateLimiter, string) {
	t.Helper()

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	svc := auth.NewService(db)
	auditor := auth.NewAuditLogger(db)
	limiter := auth.NewRateLimiter(10, time.Minute)
	key := createKey(t, svc, models.RoleAdmin, nil)

	return db, svc, auditor, limiter, key
}

func createKey(t *testing.T, svc *auth.Service, role models.Role, scope *models.KeyScope) string {
	t.Helper()

	resp, err := svc.CreateKey(&auth.CreateKeyRequest{
		Role:      role,
		Scope:     scope,
		CreatedBy: "test",
	})
	require.NoError(t, err)

	return resp.Plaintext
}

func seedJobFixtures(t *testing.T, db *gorm.DB, alias string) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()

	now := time.Now().UTC()
	triggerID := uuid.New()
	jobID := uuid.New()
	runID := uuid.New()
	backfillID := uuid.New()

	require.NoError(t, db.Create(&models.Trigger{
		ID:            triggerID,
		Type:          models.TriggerTypeCron,
		Configuration: `{"cron":"0 * * * *"}`,
		CreatedAt:     now,
		UpdatedAt:     now,
	}).Error)

	require.NoError(t, db.Create(&models.Job{
		ID:        jobID,
		Alias:     alias,
		TriggerID: triggerID,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)

	require.NoError(t, db.Create(&models.Backfill{
		ID:            backfillID,
		JobID:         jobID,
		Status:        string(models.BackfillStatusRunning),
		Start:         now,
		End:           now.Add(time.Hour),
		MaxConcurrent: 1,
		Reprocess:     string(models.ReprocessNone),
		CreatedAt:     now,
		UpdatedAt:     now,
	}).Error)

	require.NoError(t, db.Create(&models.JobRun{
		ID:          runID,
		JobID:       jobID,
		BackfillID:  &backfillID,
		TriggerID:   triggerID,
		TriggerType: string(models.TriggerTypeCron),
		Status:      "running",
		StartedAt:   now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}).Error)

	return jobID, runID, backfillID
}

func callMiddleware(
	t *testing.T,
	svc *auth.Service,
	auditor *auth.AuditLogger,
	limiter *auth.RateLimiter,
	req *http.Request,
	route *echo.RouteInfo,
	pathValues echo.PathValues,
	next echo.HandlerFunc,
) (*httptest.ResponseRecorder, error) {
	t.Helper()

	if next == nil {
		next = func(c *echo.Context) error {
			return c.String(http.StatusOK, "ok")
		}
	}

	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if route != nil {
		pv := append(echo.PathValues(nil), pathValues...)
		c.InitializeRoute(route, &pv)
	}

	handler := authmw.Auth(svc, auditor, limiter)(next)
	err := handler(c)
	return rec, err
}

func TestMiddlewareSkipsHealthEndpoint(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec, err := callMiddleware(t, svc, auditor, limiter, req, nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestMiddlewareRejectsMissingAuth(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
	_, err := callMiddleware(t, svc, auditor, limiter, req, &echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodGet}, nil, nil)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusUnauthorized, he.Code)
}

func TestMiddlewareRejectsInvalidToken(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer csk_live_invalid123")
	_, err := callMiddleware(t, svc, auditor, limiter, req, &echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodGet}, nil, nil)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusUnauthorized, he.Code)
}

func TestMiddlewareAcceptsValidToken(t *testing.T) {
	_, svc, auditor, limiter, key := setupAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec, err := callMiddleware(t, svc, auditor, limiter, req, &echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodGet}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestMiddlewareStoresAuthInContext(t *testing.T) {
	_, svc, auditor, limiter, key := setupAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+key)

	var capturedKey *models.APIKey
	rec, err := callMiddleware(
		t,
		svc,
		auditor,
		limiter,
		req,
		&echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodGet},
		nil,
		func(c *echo.Context) error {
			capturedKey = authmw.GetAuthKey(c)
			return c.String(http.StatusOK, "ok")
		},
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, capturedKey)
	require.Equal(t, models.RoleAdmin, capturedKey.Role)
}

func TestMiddlewareRejectsExpiredKey(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	svc := auth.NewService(db)
	auditor := auth.NewAuditLogger(db)
	limiter := auth.NewRateLimiter(10, time.Minute)

	past := time.Now().UTC().Add(-time.Hour)
	resp, err := svc.CreateKey(&auth.CreateKeyRequest{
		Role:      models.RoleAdmin,
		CreatedBy: "test",
		ExpiresAt: &past,
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+resp.Plaintext)
	_, err = callMiddleware(t, svc, auditor, limiter, req, &echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodGet}, nil, nil)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusUnauthorized, he.Code)
}

func TestMiddlewareRejectsRevokedKey(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	svc := auth.NewService(db)
	auditor := auth.NewAuditLogger(db)
	limiter := auth.NewRateLimiter(10, time.Minute)

	resp, err := svc.CreateKey(&auth.CreateKeyRequest{
		Role:      models.RoleAdmin,
		CreatedBy: "test",
	})
	require.NoError(t, err)
	require.NoError(t, svc.RevokeKey(resp.Key.ID))

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+resp.Plaintext)
	_, err = callMiddleware(t, svc, auditor, limiter, req, &echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodGet}, nil, nil)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusUnauthorized, he.Code)
}

func TestMiddlewareRejectsBadAuthorizationFormat(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	_, err := callMiddleware(t, svc, auditor, limiter, req, &echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodGet}, nil, nil)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusUnauthorized, he.Code)
}

func TestMiddlewareRateLimiting(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	svc := auth.NewService(db)
	auditor := auth.NewAuditLogger(db)
	limiter := auth.NewRateLimiter(2, time.Minute)

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
		req.Header.Set("Authorization", "Bearer csk_live_badkey")
		_, _ = callMiddleware(t, svc, auditor, limiter, req, &echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodGet}, nil, nil)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer csk_live_badkey")
	_, err := callMiddleware(t, svc, auditor, limiter, req, &echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodGet}, nil, nil)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusTooManyRequests, he.Code)
}

func TestMiddlewareRBACViewerCannotWrite(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	svc := auth.NewService(db)
	auditor := auth.NewAuditLogger(db)
	limiter := auth.NewRateLimiter(10, time.Minute)
	key := createKey(t, svc, models.RoleViewer, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec, err := callMiddleware(t, svc, auditor, limiter, req, &echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodGet}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)

	req = httptest.NewRequest(http.MethodPost, "/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	_, err = callMiddleware(t, svc, auditor, limiter, req, &echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodPost}, nil, nil)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, he.Code)
}

func TestMiddlewareNormalisesNamedRouteParams(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)
	key := createKey(t, svc, models.RoleViewer, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs/job-1/runs/run-2", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec, err := callMiddleware(
		t,
		svc,
		auditor,
		limiter,
		req,
		&echo.RouteInfo{Path: "/v1/jobs/:id/runs/:run_id", Method: http.MethodGet},
		echo.PathValues{
			{Name: "id", Value: "job-1"},
			{Name: "run_id", Value: "run-2"},
		},
		nil,
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestMiddlewareRejectsUnknownProtectedRoute(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)
	key := createKey(t, svc, models.RoleAdmin, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/unknown", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	_, err := callMiddleware(t, svc, auditor, limiter, req, &echo.RouteInfo{Path: "/v1/unknown", Method: http.MethodGet}, nil, nil)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, he.Code)
}

func TestMiddlewareScopedListInjectsAliases(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)
	key := createKey(t, svc, models.RoleViewer, &models.KeyScope{Jobs: []string{"alpha", "beta"}})

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+key)

	var aliases []string
	rec, err := callMiddleware(
		t,
		svc,
		auditor,
		limiter,
		req,
		&echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodGet},
		nil,
		func(c *echo.Context) error {
			aliases = authmw.GetAllowedJobAliases(c)
			return c.String(http.StatusOK, "ok")
		},
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []string{"alpha", "beta"}, aliases)
}

func TestMiddlewareScopedJobRouteAllowsInScopeAlias(t *testing.T) {
	db, svc, auditor, limiter, _ := setupAuth(t)
	jobID, _, _ := seedJobFixtures(t, db, "alpha")
	key := createKey(t, svc, models.RoleViewer, &models.KeyScope{Jobs: []string{"alpha"}})

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs/"+jobID.String(), nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec, err := callMiddleware(
		t,
		svc,
		auditor,
		limiter,
		req,
		&echo.RouteInfo{Path: "/v1/jobs/:id", Method: http.MethodGet},
		echo.PathValues{{Name: "id", Value: jobID.String()}},
		nil,
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestMiddlewareScopedJobRouteRejectsOutOfScopeAlias(t *testing.T) {
	db, svc, auditor, limiter, _ := setupAuth(t)
	jobID, _, _ := seedJobFixtures(t, db, "alpha")
	key := createKey(t, svc, models.RoleViewer, &models.KeyScope{Jobs: []string{"beta"}})

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs/"+jobID.String(), nil)
	req.Header.Set("Authorization", "Bearer "+key)
	_, err := callMiddleware(
		t,
		svc,
		auditor,
		limiter,
		req,
		&echo.RouteInfo{Path: "/v1/jobs/:id", Method: http.MethodGet},
		echo.PathValues{{Name: "id", Value: jobID.String()}},
		nil,
	)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, he.Code)
}

func TestMiddlewareScopedRunRouteAllowsInScopeAlias(t *testing.T) {
	db, svc, auditor, limiter, _ := setupAuth(t)
	jobID, runID, _ := seedJobFixtures(t, db, "alpha")
	key := createKey(t, svc, models.RoleViewer, &models.KeyScope{Jobs: []string{"alpha"}})

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs/"+jobID.String()+"/runs/"+runID.String(), nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec, err := callMiddleware(
		t,
		svc,
		auditor,
		limiter,
		req,
		&echo.RouteInfo{Path: "/v1/jobs/:id/runs/:run_id", Method: http.MethodGet},
		echo.PathValues{
			{Name: "id", Value: jobID.String()},
			{Name: "run_id", Value: runID.String()},
		},
		nil,
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestMiddlewareScopedCreateRejectsOutOfScopeAlias(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)
	key := createKey(t, svc, models.RoleOperator, &models.KeyScope{Jobs: []string{"alpha"}})

	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(`{"alias":"beta"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	_, err := callMiddleware(t, svc, auditor, limiter, req, &echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodPost}, nil, nil)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, he.Code)
}

func TestMiddlewareScopedJobdefApplyRejectsPrune(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)
	key := createKey(t, svc, models.RoleOperator, &models.KeyScope{Jobs: []string{"alpha"}})

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/jobdefs/apply",
		strings.NewReader(`{"definitions":[{"metadata":{"alias":"alpha"}}],"prune":true}`),
	)
	req.Header.Set("Authorization", "Bearer "+key)
	_, err := callMiddleware(t, svc, auditor, limiter, req, &echo.RouteInfo{Path: "/v1/jobdefs/apply", Method: http.MethodPost}, nil, nil)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, he.Code)
}

func TestMiddlewareScopedGlobalRouteRejected(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)
	key := createKey(t, svc, models.RoleViewer, &models.KeyScope{Jobs: []string{"alpha"}})

	req := httptest.NewRequest(http.MethodGet, "/v1/stats", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	_, err := callMiddleware(t, svc, auditor, limiter, req, &echo.RouteInfo{Path: "/v1/stats", Method: http.MethodGet}, nil, nil)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, he.Code)
}

func TestGetAuthKeyReturnsNilWhenNotSet(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	key := authmw.GetAuthKey(c)
	require.Nil(t, key)
}
