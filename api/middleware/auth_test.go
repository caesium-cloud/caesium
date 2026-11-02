package middleware_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
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
		Status:        models.BackfillStatusRunning,
		Start:         now,
		End:           now.Add(time.Hour),
		MaxConcurrent: 1,
		Reprocess:     models.ReprocessNone,
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
	return callMiddlewareWithDeps(t, authmw.AuthDeps{Service: svc, Auditor: auditor, Limiter: limiter}, req, route, pathValues, next)
}

func callMiddlewareWithDeps(
	t *testing.T,
	deps authmw.AuthDeps,
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

	handler := authmw.Auth(deps)(next)
	err := handler(c)
	return rec, err
}

func TestMiddlewareSkipsHealthEndpoint(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)
	rec, err := callMiddleware(t, svc, auditor, limiter, req, nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestMiddlewareRejectsMissingAuth(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/jobs", nil)
	_, err := callMiddleware(t, svc, auditor, limiter, req, &echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodGet}, nil, nil)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusUnauthorized, he.Code)
}

func TestMiddlewareRejectsInvalidToken(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer csk_live_invalid123")
	_, err := callMiddleware(t, svc, auditor, limiter, req, &echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodGet}, nil, nil)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusUnauthorized, he.Code)
}

func TestMiddlewareKeyLookupDeadlineIsServiceUnavailable(t *testing.T) {
	_, svc, auditor, _, key := setupAuth(t)
	// Two counted failures would exceed this limit. Store-unavailable lookups
	// must not count, so a later wrong key is still 401 rather than 429.
	limiter := auth.NewRateLimiter(1, time.Minute)
	const ip = "203.0.113.50"
	route := &echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodGet}

	for range 2 {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/jobs", nil)
		req.RemoteAddr = ip + ":1234"
		req.Header.Set("Authorization", "Bearer "+key)
		_, err := callMiddleware(t, svc, auditor, limiter, req, route, nil, nil)
		cancel()
		require.Error(t, err)

		he, ok := err.(*echo.HTTPError)
		require.True(t, ok)
		require.Equal(t, http.StatusServiceUnavailable, he.Code)
		require.Equal(t, "authentication store is unavailable", he.Message)
		require.ErrorIs(t, err, context.DeadlineExceeded)
	}
	require.False(t, limiter.IsLimited(ip))

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/jobs", nil)
	req.RemoteAddr = ip + ":1234"
	req.Header.Set("Authorization", "Bearer csk_live_invalid123")
	_, err := callMiddleware(t, svc, auditor, limiter, req, route, nil, nil)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusUnauthorized, he.Code)
	require.Equal(t, "invalid or expired api key", he.Message)
	require.False(t, limiter.IsLimited(ip))

	entries, err := auditor.Query(&auth.AuditQueryRequest{Action: auth.ActionAuthDenied, Limit: 10})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	requireAuditReason(t, entries[0], "invalid")
}

// TestMiddlewareLiveKeyLookupDeadlineIsServiceUnavailable is the live-request
// case the already-expired context does not prove: the caller context has no
// deadline, and the lookup itself must still end near 900ms. The callback
// stands in for dqlite retrying leader discovery until the statement context
// ends. A deleted bound would wait here until the test ceiling, not return
// 503 on its own.
func TestMiddlewareLiveKeyLookupDeadlineIsServiceUnavailable(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	var sleeps []time.Duration
	svc := auth.NewService(db,
		auth.WithValidationFailureMinLatency(5*time.Second),
		auth.WithSleep(func(d time.Duration) { sleeps = append(sleeps, d) }),
	)
	auditor := auth.NewAuditLogger(db)
	// One counted failure fills this limiter. Store unavailability must not.
	limiter := auth.NewRateLimiter(1, time.Minute)
	key := createKey(t, svc, models.RoleAdmin, nil)

	release := make(chan struct{})
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(release) }) }
	t.Cleanup(stop)

	const callbackName = "middleware-test:block-key-lookup-until-context"
	var hit bool
	block := func(tx *gorm.DB) {
		if tx.Statement == nil {
			return
		}
		if _, ok := tx.Statement.Dest.(*models.APIKey); !ok {
			return
		}
		ctx := tx.Statement.Context
		if ctx == nil {
			_ = tx.AddError(errors.New("api key lookup has no statement context"))
			return
		}
		hit = true
		select {
		case <-ctx.Done():
			_ = tx.AddError(ctx.Err())
		case <-release:
			if err := ctx.Err(); err != nil {
				_ = tx.AddError(err)
			} else {
				_ = tx.AddError(context.DeadlineExceeded)
			}
		}
	}
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callbackName, block))
	removed := false
	remove := func() {
		if removed {
			return
		}
		removed = true
		_ = db.Callback().Query().Remove(callbackName)
	}
	t.Cleanup(remove)

	const ip = "203.0.113.51"
	route := &echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodGet}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/jobs", nil)
	req.RemoteAddr = ip + ":1234"
	req.Header.Set("Authorization", "Bearer "+key)

	started := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := callMiddleware(t, svc, auditor, limiter, req, route, nil, nil)
		done <- err
	}()

	const ceiling = 2500 * time.Millisecond
	var err error
	select {
	case err = <-done:
	case <-time.After(ceiling):
		stop()
		<-done
		t.Fatal("live key lookup exceeded 2.5s; database reads must stay near 900ms and must not wait out the 5s group-refresh timeout")
	}
	elapsed := time.Since(started)

	require.True(t, hit, "the key lookup did not reach the statement-context hook")
	require.Error(t, err)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusServiceUnavailable, he.Code)
	require.Equal(t, "authentication store is unavailable", he.Message)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.GreaterOrEqual(t, elapsed, 750*time.Millisecond, "a live request must wait out the database-read bound, not an already-expired context")
	require.Less(t, elapsed, ceiling, "lookup waited %s, near the 5s group-refresh timeout", elapsed)
	require.False(t, limiter.IsLimited(ip))
	require.Empty(t, sleeps, "a timed-out lookup must not be padded to the failure-latency floor")

	remove()

	entries, err := auditor.Query(&auth.AuditQueryRequest{Action: auth.ActionAuthDenied, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, entries, "store unavailability must not write an auth-denied audit row")

	bad := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/jobs", nil)
	bad.RemoteAddr = ip + ":1234"
	bad.Header.Set("Authorization", "Bearer csk_live_invalid123")
	_, err = callMiddleware(t, svc, auditor, limiter, bad, route, nil, nil)
	require.Error(t, err)
	he, ok = err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusUnauthorized, he.Code)
	require.Equal(t, "invalid or expired api key", he.Message)
	require.False(t, limiter.IsLimited(ip))
	require.NotEmpty(t, sleeps, "a wrong key stays padded")

	entries, err = auditor.Query(&auth.AuditQueryRequest{Action: auth.ActionAuthDenied, Limit: 10})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	requireAuditReason(t, entries[0], "invalid")
}

// slowGroupFreshness admits a user after waiting on the caller context.
// synced is true so the key is re-read once the wait returns; that re-read
// must not be using a context that already died at 900ms.
type slowGroupFreshness struct {
	delay       time.Duration
	seen        context.Context
	hasDeadline bool
	remaining   time.Duration
	done        bool
}

func (s *slowGroupFreshness) Admit(ctx context.Context, user *models.User) (*models.User, bool, error) {
	s.seen = ctx
	if deadline, ok := ctx.Deadline(); ok {
		s.hasDeadline = true
		s.remaining = time.Until(deadline)
	}
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		s.done = true
		return nil, true, ctx.Err()
	case <-timer.C:
	}
	if err := ctx.Err(); err != nil {
		s.done = true
		return nil, true, err
	}
	return user, true, nil
}

// TestMiddlewareGroupRefreshIsNotCutOffByKeyLookupDeadline: a user-bound
// key whose database reads are instant and whose group admission waits 1500ms
// (between the 900ms read bound and the 5s AuthGroupRefreshTimeout) still
// authenticates when the caller context has no deadline. Admit must see that
// caller context, not a child whose deadline is ~900ms.
func TestMiddlewareGroupRefreshIsNotCutOffByKeyLookupDeadline(t *testing.T) {
	const admitDelay = 1500 * time.Millisecond
	src := mustRoleMapper(t, "viewers=viewer")
	f := newSessionFixture(t, src, &models.User{
		Email:         "nia@example.com",
		EmailVerified: true,
		Groups:        datatypes.JSON(`["viewers"]`),
		Role:          models.RoleViewer,
	})
	freshness := &slowGroupFreshness{delay: admitDelay}
	f.deps.Service = auth.NewService(f.db, auth.WithPolicySource(src), auth.WithGroupFreshness(freshness))
	key := mintUserKey(t, f)

	started := time.Now()
	got := doBearer(t, f.deps, key.Plaintext, http.MethodGet, "/v1/jobs")
	elapsed := time.Since(started)

	require.NotNil(t, freshness.seen, "group refresh was not asked to admit the user")
	require.False(t, freshness.hasDeadline, "Admit saw a child deadline (%s remaining); a caller with no deadline must not be wrapped in the 900ms database-read bound", freshness.remaining.Round(time.Millisecond))
	require.False(t, freshness.done, "Admit's context ended during a %s refresh", admitDelay)
	require.NoError(t, freshness.seen.Err())
	require.Equal(t, http.StatusOK, got.status)
	require.NotNil(t, got.principal)
	require.Equal(t, auth.PrincipalUser, got.principal.Kind)
	require.GreaterOrEqual(t, elapsed, admitDelay-100*time.Millisecond, "refresh returned in %s, as if cut off near 900ms", elapsed)
	require.Less(t, elapsed, 4*time.Second, "refresh took %s", elapsed)
}

func TestMiddlewareAcceptsValidToken(t *testing.T) {
	_, svc, auditor, limiter, key := setupAuth(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec, err := callMiddleware(t, svc, auditor, limiter, req, &echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodGet}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestMiddlewareStoresAuthInContext(t *testing.T) {
	_, svc, auditor, limiter, key := setupAuth(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+key)

	var capturedKey *models.APIKey
	var capturedPrincipal *auth.Principal
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
			capturedPrincipal = authmw.GetPrincipal(c)
			return c.String(http.StatusOK, "ok")
		},
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, capturedKey)
	require.Equal(t, models.RoleAdmin, capturedKey.Role)
	require.NotNil(t, capturedPrincipal)
	require.Equal(t, auth.PrincipalAPIKey, capturedPrincipal.Kind)
	require.Equal(t, capturedKey.KeyPrefix, capturedPrincipal.Subject)
	require.Equal(t, models.RoleAdmin, capturedPrincipal.ClusterRole())
}

func TestAuthAcceptsSessionCookie(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	svc := auth.NewService(db, auth.WithPolicySource(mustRoleMapper(t, "viewers=viewer;operators=operator")))
	auditor := auth.NewAuditLogger(db)
	limiter := auth.NewRateLimiter(10, time.Minute)
	sessions := auth.NewSessionStore(db)

	user := &models.User{
		ID:        uuid.New(),
		Issuer:    "oidc",
		Subject:   "sub-1",
		Email:     "viewer@example.com",
		Groups:    datatypes.JSON(`["viewers"]`),
		Role:      models.RoleViewer,
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, db.Create(user).Error)

	token, _, err := sessions.Create(t.Context(), auth.CreateSessionRequest{
		UserID:     user.ID,
		AuthMethod: "oidc",
		SourceIP:   "198.51.100.8",
		UserAgent:  "test",
	})
	require.NoError(t, err)

	e := echo.New()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/jobs", nil)
	req.AddCookie(&http.Cookie{Name: "caesium_session", Value: token})
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	pv := echo.PathValues(nil)
	c.InitializeRoute(&echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodGet}, &pv)

	var capturedPrincipal *auth.Principal
	handler := authmw.Auth(authmw.AuthDeps{
		Service:    svc,
		Auditor:    auditor,
		Limiter:    limiter,
		Sessions:   sessions,
		CookieName: "caesium_session",
	})(func(c *echo.Context) error {
		capturedPrincipal = authmw.GetPrincipal(c)
		return c.String(http.StatusOK, "ok")
	})

	err = handler(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, capturedPrincipal)
	require.Equal(t, auth.PrincipalUser, capturedPrincipal.Kind)
	require.Equal(t, "viewer@example.com", capturedPrincipal.Subject)
	require.Equal(t, models.RoleViewer, capturedPrincipal.ClusterRole())
	require.Nil(t, authmw.GetAuthKey(c))
}

func TestAuthAcceptsSessionCookieWithCSRFOnUnsafeMethod(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	svc := auth.NewService(db, auth.WithPolicySource(mustRoleMapper(t, "viewers=viewer;operators=operator")))
	auditor := auth.NewAuditLogger(db)
	limiter := auth.NewRateLimiter(10, time.Minute)
	sessions := auth.NewSessionStore(db)

	user := &models.User{
		ID:        uuid.New(),
		Issuer:    "oidc",
		Subject:   "sub-1",
		Email:     "operator@example.com",
		Groups:    datatypes.JSON(`["operators"]`),
		Role:      models.RoleOperator,
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, db.Create(user).Error)

	token, sess, err := sessions.Create(t.Context(), auth.CreateSessionRequest{UserID: user.ID})
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/jobs", strings.NewReader(`{"alias":"alpha"}`))
	req.AddCookie(&http.Cookie{Name: "caesium_session", Value: token})
	req.Header.Set("X-CSRF-Token", sess.CSRFToken)

	var capturedCSRF string
	rec, err := callMiddlewareWithDeps(t, authmw.AuthDeps{
		Service:    svc,
		Auditor:    auditor,
		Limiter:    limiter,
		Sessions:   sessions,
		CookieName: "caesium_session",
	}, req, &echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodPost}, nil, func(c *echo.Context) error {
		capturedCSRF = authmw.GetCSRFToken(c)
		return c.String(http.StatusOK, "ok")
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, sess.CSRFToken, capturedCSRF)
}

func TestAuthBearerPrecedenceExemptsSessionCSRF(t *testing.T) {
	db, svc, auditor, limiter, _ := setupAuth(t)
	key := createKey(t, svc, models.RoleOperator, nil)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/jobs", strings.NewReader(`{"alias":"alpha"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.AddCookie(&http.Cookie{Name: "caesium_session", Value: "bad-session-token"})

	rec, err := callMiddlewareWithDeps(t, authmw.AuthDeps{
		Service:    svc,
		Auditor:    auditor,
		Limiter:    limiter,
		Sessions:   auth.NewSessionStore(db),
		CookieName: "caesium_session",
	}, req, &echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodPost}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestAuthRejectsMalformedAuthorizationEvenWithSessionCookie(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	svc := auth.NewService(db)
	auditor := auth.NewAuditLogger(db)
	limiter := auth.NewRateLimiter(10, time.Minute)
	sessions := auth.NewSessionStore(db)
	user := &models.User{
		ID:        uuid.New(),
		Issuer:    "oidc",
		Subject:   "sub-1",
		Email:     "viewer@example.com",
		Role:      models.RoleViewer,
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, db.Create(user).Error)
	token, _, err := sessions.Create(t.Context(), auth.CreateSessionRequest{UserID: user.ID})
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/jobs", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	req.AddCookie(&http.Cookie{Name: "caesium_session", Value: token})

	called := false
	_, err = callMiddlewareWithDeps(t, authmw.AuthDeps{
		Service:    svc,
		Auditor:    auditor,
		Limiter:    limiter,
		Sessions:   sessions,
		CookieName: "caesium_session",
	}, req, &echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodGet}, nil, func(c *echo.Context) error {
		called = true
		return c.NoContent(http.StatusOK)
	})
	require.Error(t, err)
	require.False(t, called)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusUnauthorized, he.Code)

	entries, err := auditor.Query(&auth.AuditQueryRequest{Action: auth.ActionAuthDenied, Limit: 10})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "authorization", entries[0].Actor)
	requireAuditReason(t, entries[0], "malformed_authorization")
}

func TestAuthRejectsInvalidSessionCookieRecordsFailureAndAudit(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	svc := auth.NewService(db)
	auditor := auth.NewAuditLogger(db)
	limiter := auth.NewRateLimiter(1, time.Minute)
	sessions := auth.NewSessionStore(db)
	deps := authmw.AuthDeps{
		Service:    svc,
		Auditor:    auditor,
		Limiter:    limiter,
		Sessions:   sessions,
		CookieName: "caesium_session",
	}

	for range 2 {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/jobs", nil)
		req.RemoteAddr = "203.0.113.10:1234"
		req.AddCookie(&http.Cookie{Name: "caesium_session", Value: "bad-session-token"})
		_, err := callMiddlewareWithDeps(t, deps, req, &echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodGet}, nil, nil)
		require.Error(t, err)
	}

	require.True(t, limiter.IsLimited("203.0.113.10"))
	entries, err := auditor.Query(&auth.AuditQueryRequest{Action: auth.ActionAuthDenied, Limit: 10})
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, "bad-session-t", entries[0].Actor)
	requireAuditReason(t, entries[0], "session_invalid")
}

func TestAuthRejectsBadSessionCSRFRecordsFailureAndAudit(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	svc := auth.NewService(db)
	auditor := auth.NewAuditLogger(db)
	limiter := auth.NewRateLimiter(10, time.Minute)
	sessions := auth.NewSessionStore(db)
	user := &models.User{
		ID:        uuid.New(),
		Issuer:    "oidc",
		Subject:   "sub-1",
		Email:     "viewer@example.com",
		Role:      models.RoleViewer,
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, db.Create(user).Error)
	token, _, err := sessions.Create(t.Context(), auth.CreateSessionRequest{UserID: user.ID})
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/auth/logout", nil)
	req.RemoteAddr = "203.0.113.11:1234"
	req.AddCookie(&http.Cookie{Name: "caesium_session", Value: token})
	_, err = callMiddlewareWithDeps(t, authmw.AuthDeps{
		Service:    svc,
		Auditor:    auditor,
		Limiter:    limiter,
		Sessions:   sessions,
		CookieName: "caesium_session",
	}, req, &echo.RouteInfo{Path: "/auth/logout", Method: http.MethodPost}, nil, nil)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, he.Code)
	entries, err := auditor.Query(&auth.AuditQueryRequest{Action: auth.ActionAuthDenied, Limit: 10})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	requireAuditReason(t, entries[0], "csrf")
}

func requireAuditReason(t *testing.T, entry models.AuditLog, want string) {
	t.Helper()

	requireAuditMetadata(t, entry, map[string]any{"reason": want})
}

func requireAuditMetadata(t *testing.T, entry models.AuditLog, want map[string]any) {
	t.Helper()

	var metadata map[string]any
	require.NoError(t, json.Unmarshal(entry.Metadata, &metadata))
	for key, value := range want {
		require.Equal(t, value, metadata[key], "audit metadata %s", key)
	}
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

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/jobs", nil)
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

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+resp.Plaintext)
	_, err = callMiddleware(t, svc, auditor, limiter, req, &echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodGet}, nil, nil)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusUnauthorized, he.Code)
}

func TestMiddlewareRejectsBadAuthorizationFormat(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/jobs", nil)
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

	for range 3 {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/jobs", nil)
		req.Header.Set("Authorization", "Bearer csk_live_badkey")
		_, _ = callMiddleware(t, svc, auditor, limiter, req, &echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodGet}, nil, nil)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/jobs", nil)
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

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec, err := callMiddleware(t, svc, auditor, limiter, req, &echo.RouteInfo{Path: "/v1/jobs", Method: http.MethodGet}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)

	req = httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/jobs", nil)
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

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/jobs/job-1/runs/run-2", nil)
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

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/unknown", nil)
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

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/jobs", nil)
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

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/jobs/"+jobID.String(), nil)
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

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/jobs/"+jobID.String(), nil)
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

func TestMiddlewareScopedQueueCancelAllowsInScopeOperatorAndAudits(t *testing.T) {
	db, svc, auditor, limiter, _ := setupAuth(t)
	jobID, _, _ := seedJobFixtures(t, db, "alpha")
	queueID := uuid.New()
	key := createKey(t, svc, models.RoleOperator, &models.KeyScope{Jobs: []string{"alpha"}})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/v1/jobs/"+jobID.String()+"/queue/"+queueID.String(), nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec, err := callMiddleware(
		t,
		svc,
		auditor,
		limiter,
		req,
		&echo.RouteInfo{Path: "/v1/jobs/:id/queue/:queue_id", Method: http.MethodDelete},
		echo.PathValues{
			{Name: "id", Value: jobID.String()},
			{Name: "queue_id", Value: queueID.String()},
		},
		func(c *echo.Context) error {
			return c.NoContent(http.StatusNoContent)
		},
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, rec.Code)

	entries, err := auditor.Query(&auth.AuditQueryRequest{Action: auth.ActionRunQueueCancel, Limit: 10})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, auth.OutcomeSuccess, entries[0].Outcome)
	require.Equal(t, "job", entries[0].ResourceType)
	require.Equal(t, "alpha", entries[0].ResourceID)
	requireAuditMetadata(t, entries[0], map[string]any{
		"method": "DELETE",
		"path":   "/v1/jobs/:id/queue/:id",
	})
}

func TestMiddlewareScopedQueueCancelRejectsOutOfScopeOperator(t *testing.T) {
	db, svc, auditor, limiter, _ := setupAuth(t)
	jobID, _, _ := seedJobFixtures(t, db, "alpha")
	queueID := uuid.New()
	key := createKey(t, svc, models.RoleOperator, &models.KeyScope{Jobs: []string{"beta"}})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/v1/jobs/"+jobID.String()+"/queue/"+queueID.String(), nil)
	req.Header.Set("Authorization", "Bearer "+key)
	_, err := callMiddleware(
		t,
		svc,
		auditor,
		limiter,
		req,
		&echo.RouteInfo{Path: "/v1/jobs/:id/queue/:queue_id", Method: http.MethodDelete},
		echo.PathValues{
			{Name: "id", Value: jobID.String()},
			{Name: "queue_id", Value: queueID.String()},
		},
		nil,
	)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, he.Code)

	entries, err := auditor.Query(&auth.AuditQueryRequest{Action: auth.ActionAuthDenied, Limit: 10})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	requireAuditMetadata(t, entries[0], map[string]any{
		"reason":     "insufficient_scope",
		"policy_key": "DELETE /v1/jobs/:id/queue/:id",
	})
}

func TestMiddlewareQueueCancelRequiresOperator(t *testing.T) {
	db, svc, auditor, limiter, _ := setupAuth(t)
	jobID, _, _ := seedJobFixtures(t, db, "alpha")
	queueID := uuid.New()
	key := createKey(t, svc, models.RoleViewer, nil)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/v1/jobs/"+jobID.String()+"/queue/"+queueID.String(), nil)
	req.Header.Set("Authorization", "Bearer "+key)
	_, err := callMiddleware(
		t,
		svc,
		auditor,
		limiter,
		req,
		&echo.RouteInfo{Path: "/v1/jobs/:id/queue/:queue_id", Method: http.MethodDelete},
		echo.PathValues{
			{Name: "id", Value: jobID.String()},
			{Name: "queue_id", Value: queueID.String()},
		},
		nil,
	)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, he.Code)

	entries, err := auditor.Query(&auth.AuditQueryRequest{Action: auth.ActionAuthDenied, Limit: 10})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	requireAuditMetadata(t, entries[0], map[string]any{
		"reason":        "insufficient_role",
		"required_role": string(models.RoleOperator),
		"policy_key":    "DELETE /v1/jobs/:id/queue/:id",
	})
}

func TestMiddlewareScopedRunRouteAllowsInScopeAlias(t *testing.T) {
	db, svc, auditor, limiter, _ := setupAuth(t)
	jobID, runID, _ := seedJobFixtures(t, db, "alpha")
	key := createKey(t, svc, models.RoleViewer, &models.KeyScope{Jobs: []string{"alpha"}})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/jobs/"+jobID.String()+"/runs/"+runID.String(), nil)
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

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/jobs", strings.NewReader(`{"alias":"beta"}`))
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

	req := httptest.NewRequestWithContext(context.Background(),
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

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/stats", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	_, err := callMiddleware(t, svc, auditor, limiter, req, &echo.RouteInfo{Path: "/v1/stats", Method: http.MethodGet}, nil, nil)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, he.Code)
}

func TestMiddlewareLineageImpactAllowsUnscopedViewer(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)
	key := createKey(t, svc, models.RoleViewer, nil)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/lineage/impact", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec, err := callMiddleware(
		t,
		svc,
		auditor,
		limiter,
		req,
		&echo.RouteInfo{Path: "/v1/lineage/impact", Method: http.MethodGet},
		nil,
		nil,
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestMiddlewareLineageImpactRejectsScopedViewerWithSpecificMessage(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)
	key := createKey(t, svc, models.RoleViewer, &models.KeyScope{Jobs: []string{"alpha"}})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/lineage/impact", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	_, err := callMiddleware(
		t,
		svc,
		auditor,
		limiter,
		req,
		&echo.RouteInfo{Path: "/v1/lineage/impact", Method: http.MethodGet},
		nil,
		nil,
	)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, he.Code)
	require.Equal(t, authmw.LineageImpactScopedDenyMessage, he.Message)
}

func TestMiddlewareContractsGraphAllowsUnscopedViewer(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)
	key := createKey(t, svc, models.RoleViewer, nil)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/contracts/graph", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec, err := callMiddleware(
		t,
		svc,
		auditor,
		limiter,
		req,
		&echo.RouteInfo{Path: "/v1/contracts/graph", Method: http.MethodGet},
		nil,
		nil,
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestMiddlewareContractsGraphRejectsScopedViewerWithSpecificMessage(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)
	key := createKey(t, svc, models.RoleViewer, &models.KeyScope{Jobs: []string{"alpha"}})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/contracts/graph", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	_, err := callMiddleware(
		t,
		svc,
		auditor,
		limiter,
		req,
		&echo.RouteInfo{Path: "/v1/contracts/graph", Method: http.MethodGet},
		nil,
		nil,
	)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, he.Code)
	require.Equal(t, authmw.ContractsGraphScopedDenyMessage, he.Message)
}

func TestGetAuthKeyReturnsNilWhenNotSet(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	key := authmw.GetAuthKey(c)
	require.Nil(t, key)
}

func TestGetPrincipal(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	require.Nil(t, authmw.GetPrincipal(c))

	p := &auth.Principal{Kind: auth.PrincipalUser, Subject: "a@b.com"}
	c.Set(authmw.ContextKeyPrincipal, p)
	require.Equal(t, p, authmw.GetPrincipal(c))
}
