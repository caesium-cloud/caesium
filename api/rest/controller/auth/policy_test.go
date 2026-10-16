package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// policyRouter serves the two policy routes behind the real auth middleware
// (credentials + RBAC), as bind.All mounts them.
type policyRouter struct {
	echo    *echo.Echo
	auditor *iauth.AuditLogger
	keys    map[models.Role]string
}

func newPolicyRouter(t *testing.T, holder *policy.Holder) *policyRouter {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	svc := iauth.NewService(db, iauth.WithPolicyHolder(holder))
	auditor := iauth.NewAuditLogger(db)
	holder.SetObserver(iauth.PolicyReloadAuditor(auditor))
	keys := map[models.Role]string{}
	for _, role := range []models.Role{models.RoleAdmin, models.RoleOperator, models.RoleViewer} {
		resp, err := svc.CreateKey(&iauth.CreateKeyRequest{Role: role, CreatedBy: "test"})
		require.NoError(t, err)
		keys[role] = resp.Plaintext
	}

	e := echo.New()
	g := e.Group("/v1")
	g.Use(authmw.Auth(authmw.AuthDeps{
		Service:    svc,
		Auditor:    auditor,
		Limiter:    iauth.NewRateLimiter(1000, time.Minute),
		CookieName: "caesium_session",
	}))
	ctrl := New(svc, auditor)
	g.GET("/auth/policy", ctrl.GetPolicy)
	g.POST("/auth/policy/reload", ctrl.ReloadPolicy)
	return &policyRouter{echo: e, auditor: auditor, keys: keys}
}

func (r *policyRouter) do(t *testing.T, method, path string, role models.Role) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, path, nil)
	req.RemoteAddr = "198.51.100.8:1234"
	if role != "" {
		req.Header.Set("Authorization", "Bearer "+r.keys[role])
	}
	rec := httptest.NewRecorder()
	r.echo.ServeHTTP(rec, req)
	return rec
}

func writePolicyFile(t *testing.T, path, body string) {
	t.Helper()
	tmp := path + ".tmp"
	require.NoError(t, os.WriteFile(tmp, []byte(body), 0o600))
	require.NoError(t, os.Rename(tmp, path))
}

const controllerPolicy = `apiVersion: v1
kind: AccessPolicy
namespaces:
  marketing:
    secrets: {allow: ["env/MARKETING_*"]}
bindings:
  - subjects: {groups: [marketing-eng]}
    role: operator
    namespaces: [marketing]
`

func TestPolicyRoutesAreClusterAdmin(t *testing.T) {
	for _, route := range []string{"GET /v1/auth/policy", "POST /v1/auth/policy/reload"} {
		method, path, _ := strings.Cut(route, " ")
		role, ok := iauth.RequiredRole(method, path)
		require.True(t, ok, route)
		require.Equal(t, models.RoleAdmin, role, route)
	}

	r := newPolicyRouter(t, policy.NewEnvHolder(nil, "", nil))
	for _, tc := range []struct {
		method string
		path   string
	}{{http.MethodGet, "/v1/auth/policy"}, {http.MethodPost, "/v1/auth/policy/reload"}} {
		require.Equal(t, http.StatusUnauthorized, r.do(t, tc.method, tc.path, "").Code)
		require.Equal(t, http.StatusForbidden, r.do(t, tc.method, tc.path, models.RoleViewer).Code)
		require.Equal(t, http.StatusForbidden, r.do(t, tc.method, tc.path, models.RoleOperator).Code)
	}
	require.Equal(t, http.StatusOK, r.do(t, http.MethodGet, "/v1/auth/policy", models.RoleAdmin).Code)
}

// TestPolicyWithoutAFileRendersTheEnvView covers the no-file REST behaviour no
// integration lane runs (the auth lane mounts a policy file): GET renders the
// env view, and POST reload is refused with 409 and writes no audit row.
func TestPolicyWithoutAFileRendersTheEnvView(t *testing.T) {
	t.Run("SSO off: the empty single-default policy", func(t *testing.T) {
		r := newPolicyRouter(t, policy.NewEnvHolder(policy.EmptyPolicy(), "", nil))
		rec := r.do(t, http.MethodGet, "/v1/auth/policy", models.RoleAdmin)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.JSONEq(t, `{
			"apiVersion": "v1", "kind": "AccessPolicy",
			"namespaces": {"default": {}}, "bindings": []
		}`, string(readJSONField(t, rec, "policy")))
		resp := readJSONBody[PolicyResponse](t, rec)
		require.Equal(t, policy.SourceEnv, resp.Source)
		require.Empty(t, resp.Path)
		require.Empty(t, resp.Revision)
		require.Empty(t, resp.DefaultRole)
		require.Equal(t, []string{}, resp.Warnings)
		require.Nil(t, resp.LastReload)

		rec = r.do(t, http.MethodPost, "/v1/auth/policy/reload", models.RoleAdmin)
		require.Equal(t, http.StatusConflict, rec.Code)
		require.Contains(t, rec.Body.String(), "no access policy file is configured")
		rows, err := r.auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionPolicyReloaded})
		require.NoError(t, err)
		require.Empty(t, rows)
	})

	t.Run("SSO on: the translated role mapping", func(t *testing.T) {
		mapper, err := iauth.NewRoleMapper("admins=admin;*=viewer", "runner")
		require.NoError(t, err)
		view, fallback := mapper.Translated()
		r := newPolicyRouter(t, policy.NewEnvHolder(view, fallback, mapper))
		rec := r.do(t, http.MethodGet, "/v1/auth/policy", models.RoleAdmin)
		require.Equal(t, http.StatusOK, rec.Code)
		resp := readJSONBody[PolicyResponse](t, rec)
		require.Equal(t, policy.SourceEnv, resp.Source)
		require.Equal(t, models.RoleRunner, resp.DefaultRole)
		require.Len(t, resp.Policy.Bindings, 2)
		require.Equal(t, []string{"admins"}, resp.Policy.Bindings[0].Subjects.Groups)
		require.Equal(t, []string{"*"}, resp.Policy.Bindings[0].Namespaces)
		require.Equal(t, http.StatusConflict, r.do(t, http.MethodPost, "/v1/auth/policy/reload", models.RoleAdmin).Code)
	})
}

func TestPolicyWithAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	writePolicyFile(t, path, controllerPolicy)
	holder, err := policy.LoadFile(t.Context(), policy.HolderOptions{Path: path, DefaultRole: models.RoleViewer})
	require.NoError(t, err)
	r := newPolicyRouter(t, holder)
	first := policy.Hash([]byte(controllerPolicy))

	rec := r.do(t, http.MethodGet, "/v1/auth/policy", models.RoleAdmin)
	require.Equal(t, http.StatusOK, rec.Code)
	resp := readJSONBody[PolicyResponse](t, rec)
	require.Equal(t, policy.SourceFile, resp.Source)
	require.Equal(t, path, resp.Path)
	require.Equal(t, first, resp.Revision)
	require.False(t, resp.LoadedAt.IsZero())
	require.Equal(t, models.RoleViewer, resp.DefaultRole)
	require.Nil(t, resp.LastReload)
	require.Contains(t, resp.Policy.Namespaces, "default")
	require.Equal(t, []string{"env/MARKETING_*"}, resp.Policy.Namespaces["marketing"].Secrets.Allow)
	require.NotContains(t, rec.Body.String(), "apiVersion: v1", "the raw file is never returned")

	// Unchanged reload: 200, changed=false, still audited.
	rec = r.do(t, http.MethodPost, "/v1/auth/policy/reload", models.RoleAdmin)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	reload := readJSONBody[PolicyReloadResponse](t, rec)
	require.False(t, reload.Changed)
	require.Equal(t, first, reload.Revision)
	require.Equal(t, first, reload.PreviousRevision)

	// A valid change swaps the revision.
	second := controllerPolicy + "  - subjects: {groups: [admins]}\n    role: admin\n    namespaces: [\"*\"]\n"
	writePolicyFile(t, path, second)
	rec = r.do(t, http.MethodPost, "/v1/auth/policy/reload", models.RoleAdmin)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	reload = readJSONBody[PolicyReloadResponse](t, rec)
	require.True(t, reload.Changed)
	require.Equal(t, policy.Hash([]byte(second)), reload.Revision)
	require.Equal(t, first, reload.PreviousRevision)

	// An invalid file: 422 with the error and the retained revision.
	writePolicyFile(t, path, "apiVersion: v1\nkind: AccessPolicy\nunknown: 1\n")
	rec = r.do(t, http.MethodPost, "/v1/auth/policy/reload", models.RoleAdmin)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	failure := readJSONBody[PolicyReloadFailure](t, rec)
	require.Contains(t, failure.Message, `unknown field "unknown"`)
	require.Contains(t, failure.Message, path)
	require.Equal(t, policy.Hash([]byte(second)), failure.Revision)

	rec = r.do(t, http.MethodGet, "/v1/auth/policy", models.RoleAdmin)
	resp = readJSONBody[PolicyResponse](t, rec)
	require.Equal(t, policy.Hash([]byte(second)), resp.Revision, "the last good revision stays in force")
	require.NotNil(t, resp.LastReload)
	require.False(t, resp.LastReload.OK)
	require.Equal(t, policy.TriggerAPI, resp.LastReload.Trigger)
	require.Equal(t, failure.Message, resp.LastReload.Error)

	// Every API attempt is audited with the caller's key prefix as actor.
	ok, err := r.auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionPolicyReloaded})
	require.NoError(t, err)
	require.Len(t, ok, 2)
	failed, err := r.auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionPolicyReloadFailed})
	require.NoError(t, err)
	require.Len(t, failed, 1)
	require.Regexp(t, `^csk_`, failed[0].Actor)
	require.Equal(t, "198.51.100.8", failed[0].SourceIP)
}

func TestPolicyHandlersWithoutAHolder(t *testing.T) {
	ctrl, _, _ := setupControllerDeps(t)
	c, _ := newAuthContext(t, http.MethodGet, "/v1/auth/policy", "")
	err := ctrl.GetPolicy(c)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusInternalServerError, he.Code)

	c, _ = newAuthContext(t, http.MethodPost, "/v1/auth/policy/reload", "")
	err = ctrl.ReloadPolicy(c)
	he, ok = err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusInternalServerError, he.Code)
}

func readJSONField(t *testing.T, rec *httptest.ResponseRecorder, field string) json.RawMessage {
	t.Helper()
	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body[field]
}
