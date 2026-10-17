package job

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// serveCollectionGuarded serves h on a GET collection route behind the real
// auth middleware with the test-only collection guard on, as the key's
// principal. A handler that returns rows without the namespace filter fails
// the request.
func serveCollectionGuarded(t *testing.T, db *gorm.DB, key, route, target string, h echo.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	svc := iauth.NewService(db)
	deps := authmw.AuthDeps{Service: svc, Auditor: iauth.NewAuditLogger(db), Limiter: iauth.NewRateLimiter(1000, time.Minute), RequireCollectionFilter: true}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.InitializeRoute(&echo.RouteInfo{Path: route, Method: http.MethodGet}, &echo.PathValues{})
	require.NoError(t, authmw.Auth(deps)(h)(c))
	return rec
}

// guardViewerKey mints a viewer key confined to namespaces (JSON; "" keeps it
// cluster-wide).
func guardViewerKey(t *testing.T, db *gorm.DB, namespaces string) string {
	t.Helper()
	created, err := iauth.NewService(db).CreateKey(&iauth.CreateKeyRequest{Role: models.RoleViewer, CreatedBy: "test"})
	require.NoError(t, err)
	if namespaces != "" {
		require.NoError(t, db.Model(&models.APIKey{}).Where("id = ?", created.Key.ID).Update("namespaces", datatypes.JSON(namespaces)).Error)
	}
	return created.Plaintext
}

// TestJobListCollectionGuard serves the real GET /v1/jobs handler as a
// confined principal: it lists only its namespace's jobs, and each job's
// recent runs and latest run hold only runs persisted in its namespaces, so a
// moved job's old history stays with the old owner (B4).
func TestJobListCollectionGuard(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	prev := jobListService
	jobListService = func(ctx context.Context) jsvc.Job { return jsvc.ServiceWithDatabase(ctx, db) }
	t.Cleanup(func() { jobListService = prev })

	now := time.Now().UTC()
	job := func(alias, namespace string) uuid.UUID {
		id := uuid.New()
		require.NoError(t, db.Create(&models.Job{ID: id, Alias: alias, Namespace: namespace, TriggerID: uuid.New(), CreatedAt: now, UpdatedAt: now}).Error)
		return id
	}
	run := func(jobID uuid.UUID, namespace string, age time.Duration) uuid.UUID {
		id := uuid.New()
		at := now.Add(-age)
		require.NoError(t, db.Create(&models.JobRun{ID: id, JobID: jobID, Namespace: namespace, Status: "succeeded", StartedAt: at, CreatedAt: at, UpdatedAt: at}).Error)
		return id
	}
	mkt := job("guard-mkt", "marketing")
	job("guard-fin", "finance")
	moved := job("guard-moved", "marketing")
	run(mkt, "marketing", time.Hour)
	oldMoved := run(moved, "finance", 2*time.Hour) // before the job moved to marketing
	newMoved := run(moved, "marketing", time.Hour)

	type listed struct {
		ID        uuid.UUID `json:"id"`
		Alias     string    `json:"alias"`
		Namespace string    `json:"namespace"`
		LatestRun *struct {
			ID uuid.UUID `json:"id"`
		} `json:"latest_run"`
		LastRuns []struct {
			Namespace string `json:"namespace"`
		} `json:"last_runs"`
	}
	list := func(key string) map[string]listed {
		rec := serveCollectionGuarded(t, db, key, "/v1/jobs", "/v1/jobs", List)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body []listed
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		out := make(map[string]listed, len(body))
		for _, entry := range body {
			out[entry.Alias] = entry
		}
		return out
	}

	confined := list(guardViewerKey(t, db, `["marketing"]`))
	require.Len(t, confined, 2)
	require.Contains(t, confined, "guard-mkt")
	require.NotContains(t, confined, "guard-fin")
	movedEntry := confined["guard-moved"]
	require.Len(t, movedEntry.LastRuns, 1)
	require.Equal(t, "marketing", movedEntry.LastRuns[0].Namespace)
	require.NotNil(t, movedEntry.LatestRun)
	require.Equal(t, newMoved, movedEntry.LatestRun.ID)

	cluster := list(guardViewerKey(t, db, ""))
	require.Len(t, cluster, 3, "an unrestricted principal lists every job")
	require.Len(t, cluster["guard-moved"].LastRuns, 2)
	require.Equal(t, "finance", cluster["guard-moved"].LastRuns[0].Namespace, "the old owner's run %s is history an unrestricted caller still sees", oldMoved)
}

// TestEmbeddedTriggerVisible: a job response embeds its trigger for a
// confined caller only when it may read every namespace the trigger serves.
func TestEmbeddedTriggerVisible(t *testing.T) {
	prev := triggerServedNamespaces
	t.Cleanup(func() { triggerServedNamespaces = prev })
	served := map[string][]string{}
	triggerServedNamespaces = func(_ context.Context, id uuid.UUID) ([]string, error) {
		namespaces, ok := served[id.String()]
		if !ok {
			return nil, gorm.ErrRecordNotFound
		}
		return namespaces, nil
	}
	own, shared, idle, broken := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	served[own.String()] = []string{"marketing"}
	served[shared.String()] = []string{"finance", "marketing"}
	served[idle.String()] = []string{}

	marketing := authz.WithAllowedNamespaces(context.Background(), []string{"marketing"})
	for id, want := range map[uuid.UUID]bool{own: true, shared: false, idle: false, uuid.New(): false} {
		got, err := embeddedTriggerVisible(marketing, id)
		require.NoError(t, err)
		require.Equal(t, want, got, id)
	}
	both := authz.WithAllowedNamespaces(context.Background(), []string{"finance", "marketing"})
	got, err := embeddedTriggerVisible(both, shared)
	require.NoError(t, err)
	require.True(t, got)
	got, err = embeddedTriggerVisible(context.Background(), shared)
	require.NoError(t, err)
	require.True(t, got, "an unrestricted caller always gets the trigger")

	failure := errors.New("lookup failed")
	triggerServedNamespaces = func(context.Context, uuid.UUID) ([]string, error) { return nil, failure }
	_, err = embeddedTriggerVisible(marketing, broken)
	require.ErrorIs(t, err, failure, "a lookup error fails the request rather than embedding")
}
