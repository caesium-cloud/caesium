package dataset

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	svc "github.com/caesium-cloud/caesium/api/rest/service/dataset"
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
// principal (identity-and-access B6). The chain is called directly, so a
// handler that returns rows without the namespace filter fails the request
// (500) instead of passing.
func serveCollectionGuarded(t *testing.T, db *gorm.DB, key, route, target string, h echo.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	deps := authmw.AuthDeps{Service: iauth.NewService(db), Auditor: iauth.NewAuditLogger(db), Limiter: iauth.NewRateLimiter(1000, time.Minute), RequireCollectionFilter: true}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.InitializeRoute(&echo.RouteInfo{Path: route, Method: http.MethodGet}, &echo.PathValues{})
	if err := authmw.Auth(deps)(h)(c); err != nil {
		he, ok := err.(*echo.HTTPError)
		require.True(t, ok, "unexpected error %v", err)
		rec.Code = he.Code
	}
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

// seedDatasetTenancy declares "orders" (marketing producer, observed),
// "ledger" (finance producer, never observed), a source-only "raw" and an
// undeclared state "orphan", each with an active hold.
func seedDatasetTenancy(t *testing.T, db *gorm.DB) {
	t.Helper()
	now := time.Now().UTC()
	trigger := uuid.New()
	require.NoError(t, db.Create(&models.Trigger{ID: trigger, Type: models.TriggerTypeCron}).Error)
	jobs := map[string]uuid.UUID{}
	for alias, namespace := range map[string]string{"mkt-producer": "marketing", "fin-producer": "finance"} {
		id := uuid.New()
		require.NoError(t, db.Create(&models.Job{ID: id, Alias: alias, Namespace: namespace, TriggerID: trigger}).Error)
		jobs[alias] = id
	}
	for _, decl := range []struct{ alias, name, direction string }{
		{"mkt-producer", "orders", models.DatasetDirectionProduces},
		{"fin-producer", "ledger", models.DatasetDirectionProduces},
		{"mkt-producer", "raw", models.DatasetDirectionSource},
	} {
		require.NoError(t, db.Create(&models.DatasetDeclaration{ID: uuid.New(), JobID: jobs[decl.alias], JobAlias: decl.alias, Name: decl.name,
			Direction: decl.direction, CreatedAt: now, UpdatedAt: now}).Error)
	}
	for _, name := range []string{"orders", "orphan"} {
		require.NoError(t, db.Create(&models.DatasetState{ID: uuid.New(), Name: name, Status: models.DatasetStatusFresh, CreatedAt: now, UpdatedAt: now}).Error)
	}
	for _, name := range []string{"orders", "ledger", "raw", "orphan"} {
		key := "/" + name
		require.NoError(t, db.Create(&models.DatasetHold{ID: uuid.New(), Name: name, Status: models.DatasetHoldStatusActive, ActiveKey: &key,
			HeldByJobID: uuid.New(), OpenedAt: now, CreatedAt: now, UpdatedAt: now}).Error)
	}
}

func useDatabase(t *testing.T, db *gorm.DB) {
	t.Helper()
	prev := newService
	newService = func(ctx context.Context) *svc.Service { return svc.NewWithDatabase(ctx, db) }
	t.Cleanup(func() { newService = prev })
}

func sortedNames(t *testing.T, rec *httptest.ResponseRecorder, field string) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	var rows []struct {
		Name string `json:"name"`
	}
	require.NoError(t, json.Unmarshal(body[field], &rows))
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Name)
	}
	slices.Sort(names)
	return names
}

// TestDatasetListCollectionGuard is the guardedCollectionRoutes proof for
// GET /v1/datasets: a confined viewer lists only datasets with a declared
// producer in its namespaces, through the collection guard.
func TestDatasetListCollectionGuard(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	seedDatasetTenancy(t, db)
	useDatabase(t, db)
	ctrl := New()
	list := func(key string) []string {
		return sortedNames(t, serveCollectionGuarded(t, db, key, "/v1/datasets", "/v1/datasets", ctrl.List), "datasets")
	}
	require.Equal(t, []string{"orders"}, list(guardViewerKey(t, db, `["marketing"]`)))
	require.Equal(t, []string{"ledger"}, list(guardViewerKey(t, db, `["finance"]`)), "a declared, never observed dataset")
	require.Equal(t, []string{"ledger", "orders"}, list(guardViewerKey(t, db, `["marketing","finance"]`)))
	require.Equal(t, []string{"ledger", "orders", "orphan", "raw"}, list(guardViewerKey(t, db, "")), "an unrestricted principal lists every dataset")
}

// TestDatasetHoldsCollectionGuard is the guardedCollectionRoutes proof for
// GET /v1/datasets/holds.
func TestDatasetHoldsCollectionGuard(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	seedDatasetTenancy(t, db)
	useDatabase(t, db)
	ctrl := New()
	holds := func(key string) []string {
		return sortedNames(t, serveCollectionGuarded(t, db, key, "/v1/datasets/holds", "/v1/datasets/holds", ctrl.Holds), "holds")
	}
	require.Equal(t, []string{"orders"}, holds(guardViewerKey(t, db, `["marketing"]`)))
	require.Equal(t, []string{"ledger"}, holds(guardViewerKey(t, db, `["finance"]`)))
	require.Equal(t, []string{"ledger", "orders", "orphan", "raw"}, holds(guardViewerKey(t, db, "")))
}
