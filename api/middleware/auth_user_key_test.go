package middleware_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/labstack/echo/v5"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// bearerResult is what one bearer request through the auth middleware
// produced: its status, the error message when it was refused, and what the
// handler saw in the context when it ran.
type bearerResult struct {
	status    int
	message   string
	principal *auth.Principal
	key       *models.APIKey
}

func doBearer(t *testing.T, deps authmw.AuthDeps, token, method, path string) bearerResult {
	t.Helper()
	var body *strings.Reader
	if method == http.MethodGet {
		body = strings.NewReader("")
	} else {
		body = strings.NewReader(`{"alias":"alpha"}`)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, body)
	req.Header.Set("Authorization", "Bearer "+token)
	var out bearerResult
	rec, err := callMiddlewareWithDeps(t, deps, req, &echo.RouteInfo{Path: path, Method: method}, nil, func(c *echo.Context) error {
		out.principal = authmw.GetPrincipal(c)
		out.key = authmw.GetAuthKey(c)
		return c.String(http.StatusOK, "ok")
	})
	if err != nil {
		he, ok := err.(*echo.HTTPError)
		require.True(t, ok, "unexpected error %v", err)
		out.status, out.message = he.Code, he.Message
		return out
	}
	out.status = rec.Code
	return out
}

func mintUserKey(t *testing.T, f *sessionFixture) *auth.CreateKeyResponse {
	t.Helper()
	resp, err := f.deps.Service.MintUserBoundKey(t.Context(), auth.MintUserKeyRequest{UserID: f.user.ID, CreatedBy: f.user.Email})
	require.NoError(t, err)
	return resp
}

// TestAuthUserBoundKeyActsAsItsUsersSession: through the middleware a
// user-bound key is its user exactly as the user's session is (same principal
// field for field, plus the key in UserKeyID), carries no API-key context,
// and follows the user's stored groups on every request.
func TestAuthUserBoundKeyActsAsItsUsersSession(t *testing.T) {
	f := newSessionFixture(t, mustRoleMapper(t, "viewers=viewer;operators=operator"), &models.User{
		Email:         "uma@example.com",
		EmailVerified: true,
		Groups:        datatypes.JSON(`["viewers"]`),
		Role:          models.RoleAdmin, // stale denormalised role: never read for RBAC
	})
	key := mintUserKey(t, f)

	require.Equal(t, http.StatusOK, f.do(t, http.MethodGet, "/v1/jobs"))
	session := f.captured

	got := doBearer(t, f.deps, key.Plaintext, http.MethodGet, "/v1/jobs")
	require.Equal(t, http.StatusOK, got.status)
	require.Nil(t, got.key, "a user-bound key sets no API-key context, like a session")
	require.NotNil(t, got.principal.UserKeyID)
	require.Equal(t, key.Key.ID, *got.principal.UserKeyID)
	want := *session
	want.UserKeyID = got.principal.UserKeyID
	require.Equal(t, &want, got.principal)
	require.Equal(t, auth.PrincipalUser, got.principal.Kind)
	require.Nil(t, got.principal.KeyID)

	require.Equal(t, http.StatusForbidden, doBearer(t, f.deps, key.Plaintext, http.MethodPost, "/v1/jobs").status)
	entries, err := f.auditor.Query(&auth.AuditQueryRequest{Action: auth.ActionAuthDenied, Limit: 10})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "uma@example.com", entries[0].Actor, "the audit actor is the user's email, not the key prefix")
	requireAuditMetadata(t, entries[0], map[string]any{"key_role": "viewer", "reason": "insufficient_role"})

	f.setUserColumn(t, "groups", datatypes.JSON(`["operators"]`))
	got = doBearer(t, f.deps, key.Plaintext, http.MethodPost, "/v1/jobs")
	require.Equal(t, http.StatusOK, got.status)
	require.Equal(t, models.RoleOperator, got.principal.ClusterRole())

	f.setUserColumn(t, "groups", datatypes.JSON(`["nobody"]`))
	require.Equal(t, http.StatusForbidden, doBearer(t, f.deps, key.Plaintext, http.MethodGet, "/v1/jobs").status,
		"a user-bound key with no grants is refused like a grant-less session")
	whoami := doBearer(t, f.deps, key.Plaintext, http.MethodGet, "/auth/whoami")
	require.Equal(t, http.StatusOK, whoami.status, "an identity route still answers")
	require.Empty(t, whoami.principal.Grants)

	// A plain key still carries its row in the context.
	plain, err := f.deps.Service.CreateKey(&auth.CreateKeyRequest{Role: models.RoleViewer})
	require.NoError(t, err)
	got = doBearer(t, f.deps, plain.Plaintext, http.MethodGet, "/v1/jobs")
	require.Equal(t, http.StatusOK, got.status)
	require.NotNil(t, got.key)
	require.Equal(t, plain.Key.ID, got.key.ID)
	require.Equal(t, auth.PrincipalFromKey(got.key), got.principal)
}

// TestAuthUserBoundKeyRefusalsLookLikeARevokedKey: a disabled user's key, a
// revoked user-bound key and a revoked plain key all get the same 401; only
// the internal metric label and audit reason tell them apart.
func TestAuthUserBoundKeyRefusalsLookLikeARevokedKey(t *testing.T) {
	f := newSessionFixture(t, mustRoleMapper(t, "viewers=viewer"), &models.User{
		Email:         "dov@example.com",
		EmailVerified: true,
		Groups:        datatypes.JSON(`["viewers"]`),
		Role:          models.RoleViewer,
	})
	disabledKey := mintUserKey(t, f)
	revokedKey := mintUserKey(t, f)
	require.NoError(t, f.deps.Service.RevokeKey(revokedKey.Key.ID))
	plain, err := f.deps.Service.CreateKey(&auth.CreateKeyRequest{Role: models.RoleViewer})
	require.NoError(t, err)
	require.NoError(t, f.deps.Service.RevokeKey(plain.Key.ID))

	// Disabled without the cascade, so only validation stands in the way.
	f.setUserColumn(t, "disabled_at", f.user.CreatedAt)

	before := userDisabledFailures(t)
	refusedDisabled := doBearer(t, f.deps, disabledKey.Plaintext, http.MethodGet, "/v1/jobs")
	refusedRevoked := doBearer(t, f.deps, revokedKey.Plaintext, http.MethodGet, "/v1/jobs")
	refusedPlain := doBearer(t, f.deps, plain.Plaintext, http.MethodGet, "/v1/jobs")
	require.Equal(t, before+1, userDisabledFailures(t))

	for _, got := range []bearerResult{refusedDisabled, refusedRevoked, refusedPlain} {
		require.Equal(t, http.StatusUnauthorized, got.status)
		require.Equal(t, refusedPlain.message, got.message, "no user-state oracle")
		require.Nil(t, got.principal)
	}

	entries, err := f.auditor.Query(&auth.AuditQueryRequest{Action: auth.ActionAuthDenied, Limit: 10})
	require.NoError(t, err)
	reasons := map[string]string{}
	for _, entry := range entries {
		meta := map[string]any{}
		require.NoError(t, json.Unmarshal(entry.Metadata, &meta))
		reasons[entry.Actor] = meta["reason"].(string)
	}
	require.Equal(t, "user_disabled", reasons[disabledKey.Key.KeyPrefix])
	require.Equal(t, "revoked", reasons[revokedKey.Key.KeyPrefix])
	require.Equal(t, "revoked", reasons[plain.Key.KeyPrefix])
}

func userDisabledFailures(t *testing.T) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, metrics.AuthFailuresTotal.WithLabelValues("user_disabled").Write(&m))
	return m.GetCounter().GetValue()
}
