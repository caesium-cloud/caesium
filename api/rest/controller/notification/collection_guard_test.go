package notification

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	svc "github.com/caesium-cloud/caesium/api/rest/service/notification"
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

func seedSharedObjects(t *testing.T, db *gorm.DB) {
	t.Helper()
	channels := map[string]uuid.UUID{}
	for name, namespace := range map[string]string{"ch-mkt": "marketing", "ch-fin": "finance", "ch-shared": models.ClusterNamespace, "ch-default": "default"} {
		id := uuid.New()
		require.NoError(t, db.Create(&models.NotificationChannel{ID: id, Namespace: namespace, Name: name, Type: "webhook", Config: datatypes.JSON(`{}`), Enabled: true}).Error)
		channels[namespace] = id
	}
	for name, namespace := range map[string]string{"pol-mkt": "marketing", "pol-fin": "finance", "pol-shared": models.ClusterNamespace} {
		require.NoError(t, db.Create(&models.NotificationPolicy{ID: uuid.New(), Namespace: namespace, Name: name, ChannelID: channels[namespace], EventTypes: datatypes.JSON(`["run_failed"]`), Enabled: true}).Error)
	}
}

func useDatabase(t *testing.T, db *gorm.DB) {
	t.Helper()
	prev := newService
	newService = func(ctx context.Context) svc.Service { return svc.NewWithDatabase(ctx, db) }
	t.Cleanup(func() { newService = prev })
}

func listedNames(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body []struct {
		Name string `json:"name"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	names := make([]string, 0, len(body))
	for _, entry := range body {
		names = append(names, entry.Name)
	}
	slices.Sort(names)
	return names
}

// TestChannelListCollectionGuard is the guardedCollectionRoutes proof for
// GET /v1/notifications/channels: a confined viewer lists its namespaces'
// channels and the shared ones, through the collection guard.
func TestChannelListCollectionGuard(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	seedSharedObjects(t, db)
	useDatabase(t, db)
	ctrl := New(iauth.NewAuditLogger(db))

	list := func(key string) []string {
		return listedNames(t, serveCollectionGuarded(t, db, key, "/v1/notifications/channels", "/v1/notifications/channels", ctrl.ListChannels))
	}
	require.Equal(t, []string{"ch-mkt", "ch-shared"}, list(guardViewerKey(t, db, `["marketing"]`)))
	require.Equal(t, []string{"ch-fin", "ch-mkt", "ch-shared"}, list(guardViewerKey(t, db, `["marketing","finance"]`)))
	require.Equal(t, []string{"ch-default", "ch-fin", "ch-mkt", "ch-shared"}, list(guardViewerKey(t, db, "")), "an unrestricted principal lists every channel")
}

// TestPolicyListCollectionGuard is the guardedCollectionRoutes proof for
// GET /v1/notifications/policies.
func TestPolicyListCollectionGuard(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	seedSharedObjects(t, db)
	useDatabase(t, db)
	ctrl := New(iauth.NewAuditLogger(db))

	list := func(key string) []string {
		return listedNames(t, serveCollectionGuarded(t, db, key, "/v1/notifications/policies", "/v1/notifications/policies", ctrl.ListPolicies))
	}
	require.Equal(t, []string{"pol-mkt", "pol-shared"}, list(guardViewerKey(t, db, `["marketing"]`)))
	require.Equal(t, []string{"pol-fin", "pol-shared"}, list(guardViewerKey(t, db, `["finance"]`)))
	require.Equal(t, []string{"pol-fin", "pol-mkt", "pol-shared"}, list(guardViewerKey(t, db, "")))
}

// TestAuditActorFallsBackToThePrincipal: a mutation by a principal with no
// plain API key row (a session, a user-bound credential) is attributed to its
// subject; an API key keeps its prefix.
func TestAuditActorFallsBackToThePrincipal(t *testing.T) {
	c, _ := newNotificationContext(t, http.MethodPost, "/v1/notifications/channels", "", "", nil)
	require.Equal(t, "unknown", auditActor(c))

	c.Set(authmw.ContextKeyPrincipal, &iauth.Principal{Kind: iauth.PrincipalUser, Subject: "alice@example.com"})
	require.Equal(t, "alice@example.com", auditActor(c))

	c.Set(authmw.ContextKeyAuth, &models.APIKey{KeyPrefix: "csk_live_abcd"})
	require.Equal(t, "csk_live_abcd", auditActor(c), "a plain key keeps its prefix")

	c, _ = newNotificationContext(t, http.MethodPost, "/v1/notifications/channels", "", "", nil)
	c.Set(authmw.ContextKeyPrincipal, &iauth.Principal{Kind: iauth.PrincipalUser})
	require.Equal(t, "unknown", auditActor(c), "a principal without a subject")
}

// TestServiceErrorNamesAnUndeclaredNamespace: the 400 names the namespace the
// caller's own body asked for.
func TestServiceErrorNamesAnUndeclaredNamespace(t *testing.T) {
	err := serviceError(&iauth.UndeclaredNamespacesError{Namespaces: []string{"nowhere"}})
	httpErr, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusBadRequest, httpErr.Code)
	require.Equal(t, `namespace "nowhere" is not declared in the access policy`, httpErr.Message)
}
