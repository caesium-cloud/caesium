package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func mustRoleMapper(t *testing.T, mapping string) *auth.RoleMapper {
	t.Helper()
	m, err := auth.NewRoleMapper(mapping, "")
	require.NoError(t, err)
	return m
}

type sessionFixture struct {
	db       *gorm.DB
	deps     authmw.AuthDeps
	auditor  *auth.AuditLogger
	user     *models.User
	token    string
	csrf     string
	captured *auth.Principal
}

func newSessionFixture(t *testing.T, src policy.Source, user *models.User) *sessionFixture {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	var opts []auth.ServiceOption
	if src != nil {
		opts = append(opts, auth.WithPolicySource(src))
	}
	auditor := auth.NewAuditLogger(db)
	sessions := auth.NewSessionStore(db)
	user.ID = uuid.New()
	user.Issuer = "oidc"
	user.Subject = "sub-" + user.ID.String()
	user.CreatedAt = time.Now().UTC()
	require.NoError(t, db.Create(user).Error)
	token, sess, err := sessions.Create(t.Context(), auth.CreateSessionRequest{UserID: user.ID})
	require.NoError(t, err)

	return &sessionFixture{
		db:      db,
		auditor: auditor,
		user:    user,
		token:   token,
		csrf:    sess.CSRFToken,
		deps: authmw.AuthDeps{
			Service:    auth.NewService(db, opts...),
			Auditor:    auditor,
			Limiter:    auth.NewRateLimiter(100, time.Minute),
			Sessions:   sessions,
			CookieName: "caesium_session",
		},
	}
}

// do sends one cookie-session request and returns the HTTP status the
// middleware produced (200 when the handler ran).
func (f *sessionFixture) do(t *testing.T, method, path string) int {
	t.Helper()
	var body *strings.Reader
	if method == http.MethodGet {
		body = strings.NewReader("")
	} else {
		body = strings.NewReader(`{"alias":"alpha"}`)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, body)
	req.AddCookie(&http.Cookie{Name: "caesium_session", Value: f.token})
	req.Header.Set("X-CSRF-Token", f.csrf)
	f.captured = nil
	rec, err := callMiddlewareWithDeps(t, f.deps, req, &echo.RouteInfo{Path: path, Method: method}, nil, func(c *echo.Context) error {
		f.captured = authmw.GetPrincipal(c)
		return c.String(http.StatusOK, "ok")
	})
	if err != nil {
		he, ok := err.(*echo.HTTPError)
		require.True(t, ok, "unexpected error %v", err)
		return he.Code
	}
	return rec.Code
}

func (f *sessionFixture) setUserColumn(t *testing.T, column string, value any) {
	t.Helper()
	require.NoError(t, f.db.Model(&models.User{}).Where("id = ?", f.user.ID).Update(column, value).Error)
}

// TestAuthSessionResolvesGrantsPerRequest proves a session's role follows the
// stored groups on every request rather than the role frozen at login.
func TestAuthSessionResolvesGrantsPerRequest(t *testing.T) {
	f := newSessionFixture(t, mustRoleMapper(t, "viewers=viewer;operators=operator"), &models.User{
		Email:  "bob@example.com",
		Groups: datatypes.JSON(`["viewers"]`),
		Role:   models.RoleAdmin, // stale denormalised role: never read for RBAC
	})

	require.Equal(t, http.StatusOK, f.do(t, http.MethodGet, "/v1/jobs"))
	require.Equal(t, models.RoleViewer, f.captured.ClusterRole())
	require.Equal(t, http.StatusForbidden, f.do(t, http.MethodPost, "/v1/jobs"))

	f.setUserColumn(t, "groups", datatypes.JSON(`["operators"]`))
	require.Equal(t, http.StatusOK, f.do(t, http.MethodPost, "/v1/jobs"))
	require.Equal(t, models.RoleOperator, f.captured.ClusterRole())

	f.setUserColumn(t, "groups", datatypes.JSON(`["nobody"]`))
	require.Equal(t, http.StatusForbidden, f.do(t, http.MethodGet, "/v1/jobs"))
}

// TestAuthSessionUnverifiedEmailCannotUseEmailBinding is the per-request half
// of the unverified-email rule: a stored but unverified email never matches a
// users: binding.
func TestAuthSessionUnverifiedEmailCannotUseEmailBinding(t *testing.T) {
	p, err := policy.Parse([]byte(`apiVersion: v1
kind: AccessPolicy
bindings:
  - subjects: {users: [admin@example.com]}
    role: admin
    namespaces: ["*"]
`))
	require.NoError(t, err)
	f := newSessionFixture(t, &policy.Static{Policy: p}, &models.User{
		Email:         "admin@example.com",
		EmailVerified: false,
		Role:          models.RoleAdmin,
	})

	require.Equal(t, http.StatusForbidden, f.do(t, http.MethodGet, "/v1/jobs"))
	entries, err := f.auditor.Query(&auth.AuditQueryRequest{Action: auth.ActionAuthDenied, Limit: 10})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "admin@example.com", entries[0].Actor)
	requireAuditMetadata(t, entries[0], map[string]any{"key_role": "", "reason": "insufficient_role"})

	f.setUserColumn(t, "email_verified", true)
	require.Equal(t, http.StatusOK, f.do(t, http.MethodGet, "/v1/jobs"))
	require.Equal(t, models.RoleAdmin, f.captured.ClusterRole())
}

func TestAuthSessionWithoutPolicySourceIsDenied(t *testing.T) {
	f := newSessionFixture(t, nil, &models.User{
		Email:         "viewer@example.com",
		EmailVerified: true,
		Groups:        datatypes.JSON(`["viewers"]`),
		Role:          models.RoleViewer,
	})
	require.Equal(t, http.StatusForbidden, f.do(t, http.MethodGet, "/v1/jobs"))
}

func TestAuthNamespacedKeyHasNoClusterRole(t *testing.T) {
	db, svc, auditor, limiter, _ := setupAuth(t)
	created, err := svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleAdmin, CreatedBy: "test"})
	require.NoError(t, err)

	get := func() (int, *auth.Principal) {
		// A cluster route: the confined key below is refused on it whatever its
		// namespace (GET /v1/jobs went live as a collection in B4).
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/atoms", nil)
		req.Header.Set("Authorization", "Bearer "+created.Plaintext)
		var captured *auth.Principal
		rec, err := callMiddleware(t, svc, auditor, limiter, req, &echo.RouteInfo{Path: "/v1/atoms", Method: http.MethodGet}, nil,
			func(c *echo.Context) error {
				captured = authmw.GetPrincipal(c)
				return c.String(http.StatusOK, "ok")
			})
		if err != nil {
			he, ok := err.(*echo.HTTPError)
			require.True(t, ok)
			return he.Code, nil
		}
		return rec.Code, captured
	}

	status, principal := get()
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, policy.Grants{"*": models.RoleAdmin}, principal.Grants)

	setNamespaces := func(value string) {
		require.NoError(t, db.Model(&models.APIKey{}).Where("id = ?", created.Key.ID).
			Update("namespaces", datatypes.JSON(value)).Error)
	}
	setNamespaces(`["marketing"]`)
	status, _ = get()
	require.Equal(t, http.StatusForbidden, status)
	entries, err := auditor.Query(&auth.AuditQueryRequest{Action: auth.ActionAuthDenied, Limit: 10})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, created.Key.KeyPrefix, entries[0].Actor)
	requireAuditMetadata(t, entries[0], map[string]any{"key_role": "", "reason": "insufficient_role"})

	setNamespaces(`["*"]`)
	status, _ = get()
	require.Equal(t, http.StatusOK, status)

	for _, failClosed := range []string{`[]`, `null`, `{bad`} {
		setNamespaces(failClosed)
		status, _ = get()
		require.Equal(t, http.StatusForbidden, status, "namespaces %s must fail closed", failClosed)
	}
}
