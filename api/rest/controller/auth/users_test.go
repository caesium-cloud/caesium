package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type userAdminFixture struct {
	db       *gorm.DB
	ctrl     *UserAdminController
	sessions *iauth.SessionStore
	auditor  *iauth.AuditLogger
}

func newUserAdminFixture(t *testing.T) *userAdminFixture {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	sessions := iauth.NewSessionStore(db)
	auditor := iauth.NewAuditLogger(db)
	return &userAdminFixture{
		db:       db,
		ctrl:     NewUserAdmin(iauth.NewUserAdmin(sessions), auditor),
		sessions: sessions,
		auditor:  auditor,
	}
}

// seedUser stores a user carrying every credential column E1 added, so the
// views can be checked for leaks.
func (f *userAdminFixture) seedUser(t *testing.T, email string, created time.Time) *models.User {
	t.Helper()
	owner := "lease-owner"
	until := created.Add(time.Minute)
	u := &models.User{
		ID: uuid.New(), Issuer: "https://idp.example", Subject: uuid.NewString(), Email: email,
		DisplayName: "Display " + email, Groups: datatypes.JSON(`["eng","ops"]`), Role: models.RoleOperator,
		EmailVerified: true, CreatedAt: created, LastLoginAt: &created,
		RefreshGrant: []byte("sealed-grant-bytes"), RefreshGrantVersion: 4, GroupsRefreshedAt: &created,
		RefreshLeaseOwner: &owner, RefreshLeaseUntil: &until, RefreshUncertain: true,
	}
	require.NoError(t, f.db.Create(u).Error)
	return u
}

func (f *userAdminFixture) mint(t *testing.T, u *models.User) *models.Session {
	t.Helper()
	_, sess, err := f.sessions.Create(t.Context(), iauth.CreateSessionRequest{
		UserID: u.ID, AuthMethod: "oidc", SourceIP: "198.51.100.7", UserAgent: "ua", DisableGeneration: u.DisableGeneration,
	})
	require.NoError(t, err)
	return sess
}

func adminContext(t *testing.T, method, target, id string, principal *iauth.Principal) (*echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	c, rec := newAuthContext(t, method, target, "")
	if id != "" {
		c.SetPathValues(echo.PathValues{{Name: "id", Value: id}})
	}
	if principal != nil {
		c.Set(authmw.ContextKeyPrincipal, principal)
	}
	return c, rec
}

var adminKeyPrincipal = &iauth.Principal{Kind: iauth.PrincipalAPIKey, Subject: "csk_live_admin1"}

func keysOf(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var obj map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &obj))
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var (
	wantUserKeys = []string{"created_at", "disabled_at", "display_name", "email", "email_verified", "groups",
		"id", "issuer", "last_login_at", "last_login_role", "subject"}
	wantSessionKeys = []string{"absolute_expires_at", "auth_method", "created_at", "id", "idle_expires_at",
		"last_seen_at", "source_ip", "user_agent", "user_id"}
	wantUserKeyViewKeys = []string{"created_at", "description", "expires_at", "id", "key_prefix", "last_used_at"}
)

func requireHTTPStatus(t *testing.T, err error, code int) {
	t.Helper()
	var he *echo.HTTPError
	require.True(t, errors.As(err, &he), "want an HTTP error, got %v", err)
	require.Equal(t, code, he.Code)
}

func TestUserAdminViewsCarryNoCredentials(t *testing.T) {
	f := newUserAdminFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	older := f.seedUser(t, "older@example.com", now.Add(-time.Hour))
	newer := f.seedUser(t, "newer@example.com", now)
	sess := f.mint(t, newer)

	c, rec := adminContext(t, http.MethodGet, "/v1/auth/users?limit=1", "", adminKeyPrincipal)
	require.NoError(t, f.ctrl.ListUsers(c))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "2", rec.Header().Get(headerTotalCount))
	require.Equal(t, "1", rec.Header().Get(headerNextOffset))
	var list []json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list, 1)
	require.Equal(t, wantUserKeys, keysOf(t, list[0]))
	var first UserView
	require.NoError(t, json.Unmarshal(list[0], &first))
	require.Equal(t, newer.ID, first.ID)
	require.Equal(t, []string{"eng", "ops"}, first.Groups)
	require.Equal(t, models.RoleOperator, first.LastLoginRole)

	c, rec = adminContext(t, http.MethodGet, "/v1/auth/users?limit=1&offset=1", "", adminKeyPrincipal)
	require.NoError(t, f.ctrl.ListUsers(c))
	require.Empty(t, rec.Header().Get(headerNextOffset), "the last page has no continuation")
	require.Contains(t, rec.Body.String(), older.ID.String())

	userKey, err := iauth.NewService(f.db).MintUserBoundKey(t.Context(), iauth.MintUserKeyRequest{
		UserID: newer.ID, DisableGeneration: newer.DisableGeneration, Description: "laptop", CreatedBy: newer.Email,
	})
	require.NoError(t, err)

	c, rec = adminContext(t, http.MethodGet, "/v1/auth/users/"+newer.ID.String(), newer.ID.String(), adminKeyPrincipal)
	require.NoError(t, f.ctrl.GetUser(c))
	require.Equal(t, http.StatusOK, rec.Code)
	raw := rec.Body.Bytes()
	require.Equal(t, sortedCopy(append(append([]string{}, wantUserKeys...), "sessions", "sessions_total", "user_keys", "user_keys_total")), keysOf(t, raw))
	var detail struct {
		Sessions      []json.RawMessage `json:"sessions"`
		SessionsTotal int64             `json:"sessions_total"`
		UserKeys      []json.RawMessage `json:"user_keys"`
		UserKeysTotal int64             `json:"user_keys_total"`
	}
	require.NoError(t, json.Unmarshal(raw, &detail))
	require.Equal(t, int64(1), detail.SessionsTotal)
	require.Len(t, detail.Sessions, 1)
	require.Equal(t, wantSessionKeys, keysOf(t, detail.Sessions[0]))
	require.Equal(t, int64(1), detail.UserKeysTotal)
	require.Len(t, detail.UserKeys, 1)
	require.Equal(t, wantUserKeyViewKeys, keysOf(t, detail.UserKeys[0]))
	var keyView UserKeyView
	require.NoError(t, json.Unmarshal(detail.UserKeys[0], &keyView))
	require.Equal(t, userKey.Key.ID, keyView.ID)
	require.Equal(t, userKey.Key.KeyPrefix, keyView.KeyPrefix)
	require.Equal(t, "laptop", keyView.Description)
	require.NotNil(t, keyView.ExpiresAt)
	require.NotContains(t, string(raw), userKey.Key.KeyHash)
	require.NotContains(t, string(raw), userKey.Plaintext)
	require.NotContains(t, string(raw), `"role"`)

	c, rec = adminContext(t, http.MethodGet, "/v1/auth/sessions?user_id="+newer.ID.String(), "", adminKeyPrincipal)
	require.NoError(t, f.ctrl.ListSessions(c))
	require.Equal(t, "1", rec.Header().Get(headerTotalCount))
	require.Contains(t, rec.Body.String(), sess.ID.String())

	for _, body := range []string{string(raw), rec.Body.String()} {
		for _, secret := range []string{"refresh", "lease", "token_hash", "csrf", sess.TokenHash, sess.CSRFToken, "sealed-grant-bytes", "c2VhbGVk"} {
			require.NotContains(t, body, secret)
		}
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func TestUserAdminRejectsInvalidInput(t *testing.T) {
	f := newUserAdminFixture(t)
	for _, target := range []string{
		"/v1/auth/users?limit=0", "/v1/auth/users?limit=1001", "/v1/auth/users?limit=x",
		"/v1/auth/users?offset=-1", "/v1/auth/users?disabled=maybe",
	} {
		c, _ := adminContext(t, http.MethodGet, target, "", adminKeyPrincipal)
		requireHTTPStatus(t, f.ctrl.ListUsers(c), http.StatusBadRequest)
	}
	for _, target := range []string{"/v1/auth/sessions?user_id=nope", "/v1/auth/sessions?limit=1001"} {
		c, _ := adminContext(t, http.MethodGet, target, "", adminKeyPrincipal)
		requireHTTPStatus(t, f.ctrl.ListSessions(c), http.StatusBadRequest)
	}
	handlers := map[string]echo.HandlerFunc{
		"get": f.ctrl.GetUser, "disable": f.ctrl.DisableUser, "enable": f.ctrl.EnableUser, "revoke": f.ctrl.RevokeUserSessions,
	}
	for name, h := range handlers {
		c, _ := adminContext(t, http.MethodPost, "/v1/auth/users/nope", "nope", adminKeyPrincipal)
		requireHTTPStatus(t, h(c), http.StatusBadRequest)
		missing := uuid.NewString()
		c, _ = adminContext(t, http.MethodPost, "/v1/auth/users/"+missing, missing, adminKeyPrincipal)
		err := h(c)
		require.Error(t, err, name)
		requireHTTPStatus(t, err, http.StatusNotFound)
	}
	entries, err := f.auditor.Query(&iauth.AuditQueryRequest{})
	require.NoError(t, err)
	require.Empty(t, entries, "refused requests write no audit row")
}

func auditRows(t *testing.T, auditor *iauth.AuditLogger, action string) []models.AuditLog {
	t.Helper()
	rows, err := auditor.Query(&iauth.AuditQueryRequest{Action: action})
	require.NoError(t, err)
	return rows
}

func auditMeta(t *testing.T, row models.AuditLog) map[string]any {
	t.Helper()
	var meta map[string]any
	require.NoError(t, json.Unmarshal(row.Metadata, &meta))
	return meta
}

func TestUserAdminDisableEnableRevokeAudit(t *testing.T) {
	f := newUserAdminFixture(t)
	user := f.seedUser(t, "dana@example.com", time.Now().UTC())
	f.mint(t, user)
	f.mint(t, user)
	id := user.ID.String()

	// The user behind the request cannot disable itself.
	self := &iauth.Principal{Kind: iauth.PrincipalUser, Subject: user.Email, UserID: &user.ID}
	c, _ := adminContext(t, http.MethodPost, "/v1/auth/users/"+id+"/disable", id, self)
	requireHTTPStatus(t, f.ctrl.DisableUser(c), http.StatusConflict)

	c, rec := adminContext(t, http.MethodPost, "/v1/auth/users/"+id+"/disable", id, adminKeyPrincipal)
	require.NoError(t, f.ctrl.DisableUser(c))
	require.Equal(t, http.StatusOK, rec.Code)
	resp := readJSONBody[UserChangeResponse](t, rec)
	require.True(t, resp.Changed)
	require.Equal(t, int64(2), resp.SessionsRevoked)
	require.NotNil(t, resp.User.DisabledAt)

	disables := auditRows(t, f.auditor, iauth.ActionUserDisable)
	require.Len(t, disables, 1)
	require.Equal(t, adminKeyPrincipal.Subject, disables[0].Actor)
	require.Equal(t, "user", disables[0].ResourceType)
	require.Equal(t, id, disables[0].ResourceID)
	require.Equal(t, models.ClusterNamespace, disables[0].Namespace)
	require.Equal(t, iauth.OutcomeSuccess, disables[0].Outcome)
	require.Equal(t, float64(2), auditMeta(t, disables[0])["sessions_revoked"])
	revokes := auditRows(t, f.auditor, iauth.ActionSessionsRevoked)
	require.Len(t, revokes, 1)
	require.Equal(t, map[string]any{"reason": "user_disabled", "count": float64(2)}, auditMeta(t, revokes[0]))
	require.Equal(t, models.ClusterNamespace, revokes[0].Namespace)

	c, rec = adminContext(t, http.MethodPost, "/v1/auth/users/"+id+"/disable", id, adminKeyPrincipal)
	require.NoError(t, f.ctrl.DisableUser(c))
	again := readJSONBody[UserChangeResponse](t, rec)
	require.False(t, again.Changed)
	require.Zero(t, again.SessionsRevoked)
	require.Len(t, auditRows(t, f.auditor, iauth.ActionUserDisable), 1, "a no-op disable is not audited")

	c, rec = adminContext(t, http.MethodPost, "/v1/auth/users/"+id+"/enable", id, adminKeyPrincipal)
	require.NoError(t, f.ctrl.EnableUser(c))
	enabled := readJSONBody[UserChangeResponse](t, rec)
	require.True(t, enabled.Changed)
	require.Nil(t, enabled.User.DisabledAt)
	require.Len(t, auditRows(t, f.auditor, iauth.ActionUserEnable), 1)
	require.Len(t, auditRows(t, f.auditor, iauth.ActionSessionsRevoked), 1, "an enable that swept nothing writes no revocation row")

	c, rec = adminContext(t, http.MethodPost, "/v1/auth/users/"+id+"/enable", id, adminKeyPrincipal)
	require.NoError(t, f.ctrl.EnableUser(c))
	require.False(t, readJSONBody[UserChangeResponse](t, rec).Changed)
	require.Len(t, auditRows(t, f.auditor, iauth.ActionUserEnable), 1)

	// Log out everywhere: the user's own session may do it.
	current := f.row(t, user.ID)
	f.mint(t, &current)
	c, rec = adminContext(t, http.MethodPost, "/v1/auth/users/"+id+"/sessions/revoke", id, self)
	require.NoError(t, f.ctrl.RevokeUserSessions(c))
	out := readJSONBody[SessionsRevokedResponse](t, rec)
	require.Equal(t, user.ID, out.UserID)
	require.Equal(t, int64(1), out.SessionsRevoked)
	revokes = auditRows(t, f.auditor, iauth.ActionSessionsRevoked)
	require.Len(t, revokes, 2)
	require.Equal(t, map[string]any{"reason": "admin", "count": float64(1)}, auditMeta(t, revokes[0]))
	require.Equal(t, user.Email, revokes[0].Actor)

	c, rec = adminContext(t, http.MethodPost, "/v1/auth/users/"+id+"/sessions/revoke", id, adminKeyPrincipal)
	require.NoError(t, f.ctrl.RevokeUserSessions(c))
	require.Zero(t, readJSONBody[SessionsRevokedResponse](t, rec).SessionsRevoked)
	require.Len(t, auditRows(t, f.auditor, iauth.ActionSessionsRevoked), 2, "revoking nothing is not audited")
}

// TestUserAdminCascadesCountUserBoundKeys: disable and log-out-everywhere
// revoke the user's user-bound keys with its sessions and say so in the body
// and the audit rows; with no user-bound key every body is the E3 shape.
func TestUserAdminCascadesCountUserBoundKeys(t *testing.T) {
	f := newUserAdminFixture(t)
	svc := iauth.NewService(f.db)
	mintKey := func(u *models.User) *iauth.CreateKeyResponse {
		t.Helper()
		resp, err := svc.MintUserBoundKey(t.Context(), iauth.MintUserKeyRequest{UserID: u.ID, DisableGeneration: f.row(t, u.ID).DisableGeneration})
		require.NoError(t, err)
		return resp
	}

	// No user-bound key: the body keys are exactly E3's.
	plainUser := f.seedUser(t, "plain@example.com", time.Now().UTC())
	f.mint(t, plainUser)
	pid := plainUser.ID.String()
	c, rec := adminContext(t, http.MethodPost, "/v1/auth/users/"+pid+"/disable", pid, adminKeyPrincipal)
	require.NoError(t, f.ctrl.DisableUser(c))
	require.Equal(t, []string{"changed", "sessions_revoked", "user"}, keysOf(t, rec.Body.Bytes()))
	require.NotContains(t, auditMeta(t, auditRows(t, f.auditor, iauth.ActionUserDisable)[0]), "keys_revoked")
	c, rec = adminContext(t, http.MethodPost, "/v1/auth/users/"+pid+"/sessions/revoke", pid, adminKeyPrincipal)
	require.NoError(t, f.ctrl.RevokeUserSessions(c))
	require.Equal(t, []string{"sessions_revoked", "user_id"}, keysOf(t, rec.Body.Bytes()))

	// Keys only, no session: the revocation is still audited.
	user := f.seedUser(t, "keys@example.com", time.Now().UTC())
	id := user.ID.String()
	k1 := mintKey(user)
	c, rec = adminContext(t, http.MethodPost, "/v1/auth/users/"+id+"/sessions/revoke", id, adminKeyPrincipal)
	require.NoError(t, f.ctrl.RevokeUserSessions(c))
	out := readJSONBody[SessionsRevokedResponse](t, rec)
	require.Equal(t, SessionsRevokedResponse{UserID: user.ID, SessionsRevoked: 0, KeysRevoked: 1}, out)
	_, _, err := svc.AuthenticateKey(t.Context(), k1.Plaintext)
	require.ErrorIs(t, err, iauth.ErrKeyRevoked)
	revokes := auditRows(t, f.auditor, iauth.ActionSessionsRevoked)
	require.Equal(t, map[string]any{"reason": "admin", "count": float64(0), "keys_revoked": float64(1)}, auditMeta(t, revokes[0]))

	// Disable: sessions and keys in one transaction, both counted.
	mintKey(user)
	mintKey(user)
	f.mint(t, user)
	c, rec = adminContext(t, http.MethodPost, "/v1/auth/users/"+id+"/disable", id, adminKeyPrincipal)
	require.NoError(t, f.ctrl.DisableUser(c))
	resp := readJSONBody[UserChangeResponse](t, rec)
	require.True(t, resp.Changed)
	require.Equal(t, int64(1), resp.SessionsRevoked)
	require.Equal(t, int64(2), resp.KeysRevoked)
	disables := auditRows(t, f.auditor, iauth.ActionUserDisable)
	require.Equal(t, map[string]any{"sessions_revoked": float64(1), "keys_revoked": float64(2)}, auditMeta(t, disables[0]))
	revokes = auditRows(t, f.auditor, iauth.ActionSessionsRevoked)
	require.Equal(t, map[string]any{"reason": "user_disabled", "count": float64(1), "keys_revoked": float64(2)}, auditMeta(t, revokes[0]))

	c, rec = adminContext(t, http.MethodGet, "/v1/auth/users/"+id, id, adminKeyPrincipal)
	require.NoError(t, f.ctrl.GetUser(c))
	detail := readJSONBody[UserDetailView](t, rec)
	require.Empty(t, detail.UserKeys, "revoked keys are not active")
	require.NotNil(t, detail.UserKeys)
	require.Zero(t, detail.UserKeysTotal)
}

func (f *userAdminFixture) row(t *testing.T, id uuid.UUID) models.User {
	t.Helper()
	var u models.User
	require.NoError(t, f.db.Where("id = ?", id).Take(&u).Error)
	return u
}
