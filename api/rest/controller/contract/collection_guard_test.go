package contract

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	svc "github.com/caesium-cloud/caesium/api/rest/service/contract"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
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
// principal. A handler that answers a confined caller without redacting fails
// the request.
func serveCollectionGuarded(t *testing.T, db *gorm.DB, key, route, target string, h echo.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	service := iauth.NewService(db)
	deps := authmw.AuthDeps{Service: service, Auditor: iauth.NewAuditLogger(db), Limiter: iauth.NewRateLimiter(1000, time.Minute), RequireCollectionFilter: true}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.InitializeRoute(&echo.RouteInfo{Path: route, Method: http.MethodGet}, &echo.PathValues{})
	require.NoError(t, authmw.Auth(deps)(h)(c))
	return rec
}

func guardViewerKey(t *testing.T, db *gorm.DB, namespaces string) string {
	t.Helper()
	created, err := iauth.NewService(db).CreateKey(&iauth.CreateKeyRequest{Role: models.RoleViewer, CreatedBy: "test"})
	require.NoError(t, err)
	if namespaces != "" {
		require.NoError(t, db.Model(&models.APIKey{}).Where("id = ?", created.Key.ID).Update("namespaces", datatypes.JSON(namespaces)).Error)
	}
	return created.Plaintext
}

func seedObserved(t *testing.T, db *gorm.DB, alias, namespace, dataset, direction string) {
	t.Helper()
	trigger := models.Trigger{ID: uuid.New(), Type: models.TriggerTypeCron, Configuration: "{}"}
	require.NoError(t, db.Create(&trigger).Error)
	job := models.Job{ID: uuid.New(), Alias: alias, Namespace: namespace, TriggerID: trigger.ID}
	require.NoError(t, db.Create(&job).Error)
	atom := models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "busybox:1.36.1"}
	require.NoError(t, db.Create(&atom).Error)
	task := models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atom.ID, Name: "step"}
	require.NoError(t, db.Create(&task).Error)
	run := models.JobRun{ID: uuid.New(), JobID: job.ID, TriggerID: trigger.ID, Namespace: namespace, Status: "succeeded", StartedAt: time.Now().UTC()}
	require.NoError(t, db.Create(&run).Error)
	taskRun := models.TaskRun{ID: uuid.New(), JobRunID: run.ID, TaskID: task.ID, AtomID: atom.ID, Engine: models.AtomEngineDocker, Image: "busybox:1.36.1", Command: "[]", Status: "succeeded"}
	require.NoError(t, db.Create(&taskRun).Error)
	require.NoError(t, db.Create(&models.LineageDataset{ID: uuid.New(), TaskRunID: taskRun.ID, Namespace: "lake", Name: dataset, Direction: direction, CreatedAt: time.Now().UTC()}).Error)
}

// TestContractGraphCollectionGuard drives GET /v1/contracts/graph through the
// collection guard as a marketing-confined viewer: the finance consumer of a
// marketing dataset is a placeholder and the edge to it keeps only its
// endpoints (identity-and-access B5). Removing the handler's redaction mark
// fails the guard; a cluster viewer's answer is byte-identical to the
// unauthenticated handler's.
func TestContractGraphCollectionGuard(t *testing.T) {
	t.Setenv("CAESIUM_CONTRACT_ENFORCEMENT", "warn")
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	prev := contractService
	contractService = func(ctx context.Context) *svc.Service { return svc.NewWithDatabase(ctx, db) }
	t.Cleanup(func() { contractService = prev })

	seedObserved(t, db, "guard-mkt-producer", "marketing", "guard-orders", "output")
	seedObserved(t, db, "guard-fin-secret", "finance", "guard-orders", "input")

	confined := serveCollectionGuarded(t, db, guardViewerKey(t, db, `["marketing"]`), "/v1/contracts/graph", "/v1/contracts/graph", Graph)
	require.Equal(t, http.StatusOK, confined.Code, confined.Body.String())
	body := confined.Body.String()
	require.Contains(t, body, `"kind":"external"`)
	require.Contains(t, body, "guard-mkt-producer")
	require.NotContains(t, body, "guard-fin")
	require.NotContains(t, body, "guard-orders")

	cluster := serveCollectionGuarded(t, db, guardViewerKey(t, db, ""), "/v1/contracts/graph", "/v1/contracts/graph", Graph)
	require.Equal(t, http.StatusOK, cluster.Code)
	require.Contains(t, cluster.Body.String(), "guard-fin-secret")

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/contracts/graph", nil)
	rec := httptest.NewRecorder()
	require.NoError(t, Graph(echo.New().NewContext(req, rec)))
	require.Equal(t, rec.Body.String(), cluster.Body.String(), "a cluster viewer reads the unredacted bytes")
}
