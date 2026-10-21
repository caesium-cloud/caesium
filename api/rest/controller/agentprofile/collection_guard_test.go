package agentprofile

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	svc "github.com/caesium-cloud/caesium/api/rest/service/agentprofile"
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
// principal (identity-and-access D4). The chain is called directly, so a
// handler that returns rows without the namespace filter fails the request.
func serveCollectionGuarded(t *testing.T, db *gorm.DB, key, route, target string, h echo.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	deps := authmw.AuthDeps{Service: iauth.NewService(db), Auditor: iauth.NewAuditLogger(db), Limiter: iauth.NewRateLimiter(1000, time.Minute), RequireCollectionFilter: true}
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

// TestAgentProfileListCollectionGuard is the guardedCollectionRoutes proof for
// GET /v1/agentprofiles: a confined viewer lists its namespaces' profiles and
// the shared ones (the built-in triage-only among them).
func TestAgentProfileListCollectionGuard(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	require.NoError(t, svc.SeedDefaults(context.Background(), db))
	for name, namespace := range map[string]string{"prof-mkt": "marketing", "prof-fin": "finance", "prof-shared": models.ClusterNamespace} {
		require.NoError(t, db.Create(&models.AgentProfile{ID: uuid.New(), Namespace: namespace, Name: name, Image: "img", Engine: models.AtomEngineDocker}).Error)
	}
	prev := newService
	newService = func(ctx context.Context) svc.Service { return svc.NewWithDatabase(ctx, db) }
	t.Cleanup(func() { newService = prev })

	list := func(key string) []string {
		rec := serveCollectionGuarded(t, db, key, "/v1/agentprofiles", "/v1/agentprofiles", List)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body []profileView
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		names := make([]string, 0, len(body))
		for _, p := range body {
			names = append(names, p.Name)
		}
		slices.Sort(names)
		return names
	}
	require.Equal(t, []string{"prof-mkt", "prof-shared", models.DefaultTriageOnlyProfileName}, list(guardViewerKey(t, db, `["marketing"]`)))
	require.Equal(t, []string{"prof-fin", "prof-shared", models.DefaultTriageOnlyProfileName}, list(guardViewerKey(t, db, `["finance"]`)))
	require.Equal(t, []string{"prof-fin", "prof-mkt", "prof-shared", models.DefaultTriageOnlyProfileName}, list(guardViewerKey(t, db, "")))
}

func TestServiceErrorNamesAnUndeclaredNamespace(t *testing.T) {
	err := serviceError(&iauth.UndeclaredNamespacesError{Namespaces: []string{"nowhere"}})
	httpErr, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusBadRequest, httpErr.Code)
	require.Equal(t, `namespace "nowhere" is not declared in the access policy`, httpErr.Message)
}
