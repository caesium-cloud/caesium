package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// keyRoutesServer serves the real key handlers behind the real auth
// middleware, with the test-only collection guard on: a confined principal's
// successful key list must have been namespace-filtered, or the middleware
// returns errCollectionUnfiltered (api/auth_rbac_policy_completeness_test.go
// guardedCollectionRoutes). The guard runs after the handler has committed
// its 200, and echo's default error handler drops an error on a committed
// response, so the server's error handler records such a late error and do
// fails the test on it.
type keyRoutesServer struct {
	db       *gorm.DB
	svc      *iauth.Service
	e        *echo.Echo
	sessions *iauth.SessionStore
	// lateErr is the error the middleware chain returned after the response
	// was committed during the last request, if any.
	lateErr error
}

func newKeyRoutesServer(t *testing.T) *keyRoutesServer {
	t.Helper()
	return newKeyRoutesServerListing(t, (*Controller).ListKeys)
}

// newKeyRoutesServerListing mounts list as GET /v1/auth/keys, so the
// harness's self-test can serve a list handler that forgets the filter.
func newKeyRoutesServerListing(t *testing.T, list func(*Controller, *echo.Context) error) *keyRoutesServer {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	p, err := policy.Parse([]byte(`apiVersion: v1
kind: AccessPolicy
namespaces:
  marketing: {}
  finance: {}
bindings:
  - subjects: {groups: [mkt-key-admins]}
    role: admin
    namespaces: [marketing]
`))
	require.NoError(t, err)
	svc := iauth.NewService(db, iauth.WithPolicySource(&policy.Static{Policy: p}))
	auditor := iauth.NewAuditLogger(db)
	sessions := iauth.NewSessionStore(db)
	ctrl := New(svc, auditor)

	s := &keyRoutesServer{db: db, svc: svc, sessions: sessions}
	s.e = echo.New()
	render := echo.DefaultHTTPErrorHandler(false)
	s.e.HTTPErrorHandler = func(c *echo.Context, err error) {
		if resp, _ := echo.UnwrapResponse(c.Response()); resp != nil && resp.Committed {
			s.lateErr = err
			return
		}
		render(c, err)
	}
	g := s.e.Group("/v1", authmw.Auth(authmw.AuthDeps{
		Service: svc, Auditor: auditor, Limiter: iauth.NewRateLimiter(1000, time.Minute),
		Sessions: sessions, CookieName: "caesium_session", RequireCollectionFilter: true,
	}))
	g.GET("/auth/keys", func(c *echo.Context) error { return list(ctrl, c) })
	g.POST("/auth/keys", ctrl.CreateKey)
	g.POST("/auth/keys/:id/revoke", ctrl.RevokeKey)
	g.POST("/auth/keys/:id/rotate", ctrl.RotateKey)
	return s
}

// key seeds a key with role and namespaces stored verbatim ("" = SQL NULL).
func (s *keyRoutesServer) key(t *testing.T, role models.Role, namespaces string) (string, *models.APIKey) {
	t.Helper()
	created, err := s.svc.CreateKey(&iauth.CreateKeyRequest{Role: role, CreatedBy: "seed", Namespaces: datatypes.JSON(namespaces)})
	require.NoError(t, err)
	if namespaces == "" {
		require.NoError(t, s.db.Model(&models.APIKey{}).Where("id = ?", created.Key.ID).Update("namespaces", nil).Error)
	}
	return created.Plaintext, created.Key
}

type keyRequest struct {
	method, path, body string
	bearer             string
	cookie, csrf       string
}

// do serves r and fails the test if the middleware chain returned an error
// after the response was committed (the collection guard's failure).
func (s *keyRoutesServer) do(t *testing.T, r keyRequest) (int, string) {
	t.Helper()
	status, body, late := s.serve(t, r)
	require.NoError(t, late, "%s %s: the middleware failed the request after the handler committed %d", r.method, r.path, status)
	return status, body
}

// serve is do without the check: it also returns the late error.
func (s *keyRoutesServer) serve(t *testing.T, r keyRequest) (int, string, error) {
	t.Helper()
	s.lateErr = nil
	req := httptest.NewRequestWithContext(context.Background(), r.method, r.path, strings.NewReader(r.body))
	if r.body != "" {
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	if r.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+r.bearer)
	}
	if r.cookie != "" {
		req.AddCookie(&http.Cookie{Name: "caesium_session", Value: r.cookie})
		if r.csrf != "" {
			req.Header.Set("X-CSRF-Token", r.csrf)
		}
	}
	rec := httptest.NewRecorder()
	s.e.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String(), s.lateErr
}

func (s *keyRoutesServer) lastDenial(t *testing.T) models.AuditLog {
	t.Helper()
	var entry models.AuditLog
	require.NoError(t, s.db.Where("action = ?", iauth.ActionAuthDenied).Order("timestamp DESC").First(&entry).Error)
	return entry
}

func listedIDs(t *testing.T, body string) []string {
	t.Helper()
	var keys []models.APIKey
	require.NoError(t, json.Unmarshal([]byte(body), &keys), body)
	ids := make([]string, 0, len(keys))
	for _, key := range keys {
		ids = append(ids, key.ID.String())
	}
	return ids
}

// TestKeyRoutesHarnessSeesAnUnfilteredList proves the harness can fail: a
// list handler that returns every key without the namespace filter commits
// its 200, and the collection guard's error still reaches the test. Without
// this, TestKeyRoutesCollectionGuard would pass with the filter mark removed.
func TestKeyRoutesHarnessSeesAnUnfilteredList(t *testing.T) {
	forgetful := func(ctrl *Controller, c *echo.Context) error {
		keys, err := ctrl.service.ListKeys()
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, keys)
	}
	s := newKeyRoutesServerListing(t, forgetful)
	mktAdmin, _ := s.key(t, models.RoleAdmin, `["marketing"]`)
	clusterAdmin, _ := s.key(t, models.RoleAdmin, "")

	status, _, late := s.serve(t, keyRequest{method: http.MethodGet, path: "/v1/auth/keys", bearer: mktAdmin})
	require.Equal(t, http.StatusOK, status, "the handler committed its 200 before the guard ran")
	var he *echo.HTTPError
	require.ErrorAs(t, late, &he, "the collection guard's failure must reach the test")
	require.Equal(t, http.StatusInternalServerError, he.Code)
	require.Contains(t, he.Message, "did not apply the namespace filter")

	_, _, late = s.serve(t, keyRequest{method: http.MethodGet, path: "/v1/auth/keys", bearer: clusterAdmin})
	require.NoError(t, late, "an unrestricted principal has nothing to filter")
}

// TestKeyRoutesCollectionGuard is the guardedCollectionRoutes proof for
// GET /v1/auth/keys (it fails if ListKeys stops marking its response
// filtered; see TestKeyRoutesHarnessSeesAnUnfilteredList), and drives the
// rest of the key surface as a namespace admin through the real middleware
// and handlers.
func TestKeyRoutesCollectionGuard(t *testing.T) {
	s := newKeyRoutesServer(t)
	mktAdmin, mktAdminKey := s.key(t, models.RoleAdmin, `["marketing"]`)
	clusterAdmin, clusterAdminKey := s.key(t, models.RoleAdmin, "")
	_, mktRunner := s.key(t, models.RoleRunner, `["marketing"]`)
	_, finRunner := s.key(t, models.RoleRunner, `["finance"]`)
	_, bothRunner := s.key(t, models.RoleRunner, `["finance","marketing"]`)
	_, brokenKey := s.key(t, models.RoleRunner, `{bad`)

	// The namespace admin lists only keys confined to marketing.
	status, body := s.do(t, keyRequest{method: http.MethodGet, path: "/v1/auth/keys", bearer: mktAdmin})
	require.Equal(t, http.StatusOK, status, body)
	require.ElementsMatch(t, []string{mktAdminKey.ID.String(), mktRunner.ID.String()}, listedIDs(t, body))

	// The cluster admin's list is every key, unfiltered.
	status, body = s.do(t, keyRequest{method: http.MethodGet, path: "/v1/auth/keys", bearer: clusterAdmin})
	require.Equal(t, http.StatusOK, status, body)
	require.ElementsMatch(t, []string{mktAdminKey.ID.String(), clusterAdminKey.ID.String(), mktRunner.ID.String(),
		finRunner.ID.String(), bothRunner.ID.String(), brokenKey.ID.String()}, listedIDs(t, body))

	// Mint inside marketing; refused anywhere wider, with an audit row.
	status, body = s.do(t, keyRequest{method: http.MethodPost, path: "/v1/auth/keys", bearer: mktAdmin,
		body: `{"role":"operator","namespaces":["marketing"]}`})
	require.Equal(t, http.StatusCreated, status, body)
	var created createKeyResponse
	require.NoError(t, json.Unmarshal([]byte(body), &created))
	require.JSONEq(t, `["marketing"]`, string(created.APIKey.Namespaces))
	require.Equal(t, mktAdminKey.KeyPrefix, created.APIKey.CreatedBy)
	for _, tc := range []struct{ body, namespace string }{
		{`{"role":"operator","namespaces":["finance"]}`, "finance"},
		{`{"role":"viewer"}`, "*"},
	} {
		status, body = s.do(t, keyRequest{method: http.MethodPost, path: "/v1/auth/keys", bearer: mktAdmin, body: tc.body})
		require.Equal(t, http.StatusForbidden, status, body)
		require.Equal(t, tc.namespace, s.lastDenial(t).Namespace)
	}

	// Rotate and revoke: marketing keys only.
	status, body = s.do(t, keyRequest{method: http.MethodPost, path: "/v1/auth/keys/" + mktRunner.ID.String() + "/rotate", bearer: mktAdmin, body: `{}`})
	require.Equal(t, http.StatusCreated, status, body)
	for _, target := range []*models.APIKey{finRunner, bothRunner, brokenKey, clusterAdminKey} {
		for _, verb := range []string{"rotate", "revoke"} {
			status, body = s.do(t, keyRequest{method: http.MethodPost, path: "/v1/auth/keys/" + target.ID.String() + "/" + verb, bearer: mktAdmin, body: `{}`})
			require.Equal(t, http.StatusForbidden, status, "%s %s: %s", verb, target.Namespaces, body)
		}
	}
	status, body = s.do(t, keyRequest{method: http.MethodPost, path: "/v1/auth/keys/" + mktRunner.ID.String() + "/revoke", bearer: mktAdmin})
	require.Equal(t, http.StatusOK, status, body)

	// The cluster admin cleans up the unusable key; rotating it is refused.
	status, body = s.do(t, keyRequest{method: http.MethodPost, path: "/v1/auth/keys/" + brokenKey.ID.String() + "/rotate", bearer: clusterAdmin, body: `{}`})
	require.Equal(t, http.StatusConflict, status, body)
	status, body = s.do(t, keyRequest{method: http.MethodPost, path: "/v1/auth/keys/" + brokenKey.ID.String() + "/revoke", bearer: clusterAdmin})
	require.Equal(t, http.StatusOK, status, body)
}

// TestKeyRoutesSSONamespaceAdmin: an SSO user whose policy grant is admin in
// marketing manages marketing keys with its cookie session (the A5 fix for
// SSO admins being 401'd), and CSRF still guards the unsafe requests.
func TestKeyRoutesSSONamespaceAdmin(t *testing.T) {
	s := newKeyRoutesServer(t)
	now := time.Now().UTC()
	user := &models.User{ID: uuid.New(), Issuer: "oidc", Subject: "alice", Email: "alice@example.com", EmailVerified: true,
		Groups: datatypes.JSON(`["mkt-key-admins"]`), Role: models.RoleViewer, CreatedAt: now}
	require.NoError(t, s.db.Create(user).Error)
	token, sess, err := s.sessions.Create(t.Context(), iauth.CreateSessionRequest{UserID: user.ID})
	require.NoError(t, err)

	status, body := s.do(t, keyRequest{method: http.MethodPost, path: "/v1/auth/keys", cookie: token,
		body: `{"role":"runner","namespaces":["marketing"]}`})
	require.Equal(t, http.StatusForbidden, status, "a cookie POST without the CSRF token is refused: %s", body)

	status, body = s.do(t, keyRequest{method: http.MethodPost, path: "/v1/auth/keys", cookie: token, csrf: sess.CSRFToken,
		body: `{"role":"runner","namespaces":["marketing"]}`})
	require.Equal(t, http.StatusCreated, status, body)
	var created createKeyResponse
	require.NoError(t, json.Unmarshal([]byte(body), &created))
	require.Equal(t, "alice@example.com", created.APIKey.CreatedBy)
	var audit models.AuditLog
	require.NoError(t, s.db.Where("action = ? AND resource_id = ?", iauth.ActionKeyCreate, created.APIKey.ID.String()).First(&audit).Error)
	require.Equal(t, "alice@example.com", audit.Actor)
	require.Equal(t, "marketing", audit.Namespace)

	status, body = s.do(t, keyRequest{method: http.MethodPost, path: "/v1/auth/keys", cookie: token, csrf: sess.CSRFToken,
		body: `{"role":"runner","namespaces":["finance"]}`})
	require.Equal(t, http.StatusForbidden, status, body)
	require.Equal(t, "alice@example.com", s.lastDenial(t).Actor)

	status, body = s.do(t, keyRequest{method: http.MethodGet, path: "/v1/auth/keys", cookie: token})
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, []string{created.APIKey.ID.String()}, listedIDs(t, body))

	status, body = s.do(t, keyRequest{method: http.MethodPost, path: "/v1/auth/keys/" + created.APIKey.ID.String() + "/revoke",
		cookie: token, csrf: sess.CSRFToken})
	require.Equal(t, http.StatusOK, status, body)
	var revokeAudit models.AuditLog
	require.NoError(t, s.db.Where("action = ? AND resource_id = ?", iauth.ActionKeyRevoke, created.APIKey.ID.String()).First(&revokeAudit).Error)
	require.Equal(t, "alice@example.com", revokeAudit.Actor)
}
