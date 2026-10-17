//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// User and session administration (identity-and-access E3) through the real
// CLI and REST surface. SSO users exist only after an SSO login, which the
// api-key lanes cannot drive (the Keycloak lane, G3, will), so each scenario
// seeds its users and sessions through the live catalog, the way
// TestAuthSessionCookieWithoutGrantsIsRefused does, and asserts only through
// the server.

// userAdminRoutes are the six E3 routes; %s is a user id.
var userAdminRoutes = []struct{ method, path string }{
	{http.MethodGet, "/v1/auth/users"},
	{http.MethodGet, "/v1/auth/users/%s"},
	{http.MethodPost, "/v1/auth/users/%s/disable"},
	{http.MethodPost, "/v1/auth/users/%s/enable"},
	{http.MethodPost, "/v1/auth/users/%s/sessions/revoke"},
	{http.MethodGet, "/v1/auth/sessions"},
}

func userAdminPath(path string, id uuid.UUID) string {
	if strings.Contains(path, "%s") {
		return fmt.Sprintf(path, id)
	}
	return path
}

type adminUserDoc struct {
	ID            string     `json:"id"`
	Issuer        string     `json:"issuer"`
	Email         string     `json:"email"`
	LastLoginRole string     `json:"last_login_role"`
	DisabledAt    *time.Time `json:"disabled_at"`
}

type adminSessionDoc struct {
	ID     string `json:"id"`
	UserID string `json:"user_id"`
}

type adminUserDetailDoc struct {
	adminUserDoc
	Sessions      []json.RawMessage `json:"sessions"`
	SessionsTotal int64             `json:"sessions_total"`
}

type adminUserChangeDoc struct {
	User            adminUserDoc `json:"user"`
	Changed         bool         `json:"changed"`
	SessionsRevoked int64        `json:"sessions_revoked"`
}

type adminAuditRow struct {
	Actor        string          `json:"actor"`
	Action       string          `json:"action"`
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id"`
	Outcome      string          `json:"outcome"`
	Namespace    string          `json:"namespace"`
	Metadata     json.RawMessage `json:"metadata"`
}

// userAdminFixture seeds users of one unique issuer and mints their sessions
// with the lane's hash secret, exactly as a login would.
type userAdminFixture struct {
	issuer   string
	stamp    int64
	conn     *gorm.DB
	sessions *iauth.SessionStore
}

func (s *IntegrationTestSuite) newUserAdminFixture(label string) *userAdminFixture {
	s.T().Helper()
	vars := env.Variables()
	s.Require().NotEmpty(vars.AuthKeyHashSecret, "the auth lane runner must carry CAESIUM_AUTH_KEY_HASH_SECRET")
	conn := s.openIntegrationCatalogGorm()
	f := &userAdminFixture{
		stamp:    time.Now().UnixNano(),
		conn:     conn,
		sessions: iauth.NewSessionStore(conn, iauth.WithSessionHashSecret(vars.AuthKeyHashSecret)),
	}
	f.issuer = fmt.Sprintf("integration-e3-%s-%d", label, f.stamp)
	s.T().Cleanup(func() {
		s.NoError(conn.Exec(`DELETE FROM sessions WHERE user_id IN (SELECT id FROM users WHERE issuer = ?)`, f.issuer).Error)
		s.NoError(conn.Exec(`DELETE FROM users WHERE issuer = ?`, f.issuer).Error)
	})
	return f
}

func (s *IntegrationTestSuite) seedAdminUser(f *userAdminFixture, name string, created time.Time, groups ...string) *models.User {
	s.T().Helper()
	if groups == nil {
		groups = []string{}
	}
	raw, err := json.Marshal(groups)
	s.Require().NoError(err)
	user := &models.User{
		ID:          uuid.New(),
		Issuer:      f.issuer,
		Subject:     fmt.Sprintf("%s-%d", name, f.stamp),
		Email:       fmt.Sprintf("%s-%d@e3.example.com", name, f.stamp),
		Groups:      datatypes.JSON(raw),
		Role:        models.RoleViewer,
		CreatedAt:   created,
		LastLoginAt: &created,
	}
	s.Require().NoError(f.conn.Create(user).Error)
	return user
}

// mintSession creates a session for user at its current version.
func (s *IntegrationTestSuite) mintSession(f *userAdminFixture, userID uuid.UUID) (*http.Cookie, *models.Session) {
	s.T().Helper()
	var row models.User
	s.Require().NoError(f.conn.Where("id = ?", userID).Take(&row).Error)
	token, sess, err := f.sessions.Create(s.T().Context(), iauth.CreateSessionRequest{
		UserID:            userID,
		AuthMethod:        "oidc",
		SourceIP:          "127.0.0.1",
		UserAgent:         "integration-e3",
		DisableGeneration: row.DisableGeneration,
	})
	s.Require().NoError(err)
	return &http.Cookie{Name: env.Variables().AuthSessionCookieName, Value: token}, sess
}

// adminHTTP sends one request with an optional bearer token, cookie and
// headers, and returns status, body and response headers.
func (s *IntegrationTestSuite) adminHTTP(method, path, token string, cookie *http.Cookie, headers map[string]string) (int, string, http.Header) {
	s.T().Helper()
	req, err := http.NewRequestWithContext(s.T().Context(), method, s.caesiumURL+path, nil)
	s.Require().NoError(err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	return resp.StatusCode, string(body), resp.Header
}

// userAuditRows reads `caesium auth audit --action action` (always JSON) and
// keeps the rows about userID.
func (s *IntegrationTestSuite) userAuditRows(action string, userID uuid.UUID) []adminAuditRow {
	s.T().Helper()
	rows := runCLIJSON[[]adminAuditRow](s, "auth", "audit", "--action", action, "--limit", "500", "--server", s.caesiumURL)
	var out []adminAuditRow
	for _, row := range rows {
		if row.ResourceID == userID.String() {
			out = append(out, row)
		}
	}
	return out
}

func (s *IntegrationTestSuite) requireAuditMeta(row adminAuditRow, want map[string]any) {
	s.T().Helper()
	var got map[string]any
	s.Require().NoError(json.Unmarshal(row.Metadata, &got), string(row.Metadata))
	s.Equal(want, got)
}

func listedIDs(users []adminUserDoc) []string {
	ids := make([]string, 0, len(users))
	for _, u := range users {
		ids = append(ids, u.ID)
	}
	return ids
}

func jsonKeys(s *IntegrationTestSuite, raw json.RawMessage) []string {
	var obj map[string]json.RawMessage
	s.Require().NoError(json.Unmarshal(raw, &obj), string(raw))
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// TestAuthUserAdministrationLifecycle drives list/get/disable/enable/logout
// through the CLI and REST with the bootstrap admin key: filters and paging,
// views without secret material, disable revoking the live cookie and being
// idempotent, the audit rows, enable not reviving revoked cookies, and
// log-out-everywhere.
func (s *IntegrationTestSuite) TestAuthUserAdministrationLifecycle() {
	s.requireAuthLane()
	f := s.newUserAdminFixture("life")
	now := time.Now().UTC().Truncate(time.Second)
	oldest := s.seedAdminUser(f, "wren", now.Add(-2*time.Minute))
	middle := s.seedAdminUser(f, "vale", now.Add(-time.Minute))
	user := s.seedAdminUser(f, "uma", now, "eng")
	cookieA, sessA := s.mintSession(f, user.ID)
	cookieB, sessB := s.mintSession(f, user.ID)
	server := "--server"

	status, body := s.requestWithCookie(http.MethodGet, "/auth/whoami", cookieA)
	s.Require().Equal(http.StatusOK, status, "the seeded session is live: %s", body)

	// Disable middle through the API first so --disabled has both states.
	_ = runCLIJSON[adminUserChangeDoc](s, "auth", "user", "disable", middle.ID.String(), "--json", server, s.caesiumURL)

	// List: --issuer orders newest first; the JSON is exactly the view.
	stdout, err := s.runCLIStdout("auth", "user", "list", "--issuer", f.issuer, "--json", server, s.caesiumURL)
	s.Require().NoError(err, stdout)
	var rawUsers []json.RawMessage
	s.Require().NoError(decodeCLIJSONStdout([]byte(stdout), &rawUsers), stdout)
	s.Require().Len(rawUsers, 3)
	s.Equal([]string{"created_at", "disabled_at", "display_name", "email", "email_verified", "groups", "id",
		"issuer", "last_login_at", "last_login_role", "subject"}, jsonKeys(s, rawUsers[0]))
	for _, secret := range []string{"refresh", "lease", "token", "csrf"} {
		s.NotContains(stdout, secret, "a user view must carry no credential material")
	}
	all := runCLIJSON[[]adminUserDoc](s, "auth", "user", "list", "--issuer", f.issuer, "--json", server, s.caesiumURL)
	s.Equal([]string{user.ID.String(), middle.ID.String(), oldest.ID.String()}, listedIDs(all))

	disabledOnly := runCLIJSON[[]adminUserDoc](s, "auth", "user", "list", "--issuer", f.issuer, "--disabled", "true", "--json", server, s.caesiumURL)
	s.Equal([]string{middle.ID.String()}, listedIDs(disabledOnly))
	enabledOnly := runCLIJSON[[]adminUserDoc](s, "auth", "user", "list", "--issuer", f.issuer, "--disabled", "false", "--json", server, s.caesiumURL)
	s.Equal([]string{user.ID.String(), oldest.ID.String()}, listedIDs(enabledOnly))
	byEmail := runCLIJSON[[]adminUserDoc](s, "auth", "user", "list", "--email", strings.ToUpper(fmt.Sprintf("uma-%d@", f.stamp)), "--json", server, s.caesiumURL)
	s.Equal([]string{user.ID.String()}, listedIDs(byEmail), "email filters case-insensitively")

	// Paging through the CLI (continuation on stderr) and REST (headers).
	pageOut, pageErr, err := s.runCLISeparate("auth", "user", "list", "--issuer", f.issuer, "--limit", "1", "--offset", "1", "--json", server, s.caesiumURL)
	s.Require().NoError(err, pageErr)
	var page []adminUserDoc
	s.Require().NoError(decodeCLIJSONStdout([]byte(pageOut), &page), pageOut)
	s.Equal([]string{middle.ID.String()}, listedIDs(page))
	s.Contains(pageErr, "3 users in total; next page: --offset 2")
	status, body, header := s.adminHTTP(http.MethodGet, "/v1/auth/users?issuer="+f.issuer+"&limit=2", s.authAPIKey, nil, nil)
	s.Require().Equal(http.StatusOK, status, body)
	s.Equal("3", header.Get("X-Caesium-Total-Count"))
	s.Equal("2", header.Get("X-Caesium-Next-Offset"))
	status, body, header = s.adminHTTP(http.MethodGet, "/v1/auth/users?issuer="+f.issuer+"&limit=2&offset=2", s.authAPIKey, nil, nil)
	s.Require().Equal(http.StatusOK, status, body)
	s.Empty(header.Get("X-Caesium-Next-Offset"), "the last page has no continuation")
	s.Contains(body, oldest.ID.String())

	// The table output names the user.
	table, err := s.runCLIStdout("auth", "user", "list", "--issuer", f.issuer, server, s.caesiumURL)
	s.Require().NoError(err, table)
	s.Contains(table, "LAST LOGIN ROLE")
	s.Contains(table, user.Email)

	// Invalid input is a 400; an unknown user a 404.
	for _, path := range []string{
		"/v1/auth/users?limit=0", "/v1/auth/users?limit=1001", "/v1/auth/users?offset=-1",
		"/v1/auth/users?disabled=maybe", "/v1/auth/users/not-a-uuid", "/v1/auth/sessions?user_id=not-a-uuid",
	} {
		status, body, _ = s.adminHTTP(http.MethodGet, path, s.authAPIKey, nil, nil)
		s.Equal(http.StatusBadRequest, status, "%s: %s", path, body)
	}
	status, body, _ = s.adminHTTP(http.MethodPost, "/v1/auth/users/"+uuid.NewString()+"/disable", s.authAPIKey, nil, nil)
	s.Equal(http.StatusNotFound, status, body)
	for _, bad := range [][]string{{"--limit", "0"}, {"--offset", "-1"}} {
		args := append([]string{"auth", "user", "list", "--json"}, bad...)
		badOut, badErr, err := s.runCLISeparate(append(args, server, s.caesiumURL)...)
		s.Require().Error(err, "caesium %v must fail, not fall back to a default page", bad)
		s.Contains(badErr, "(400)")
		s.Empty(strings.TrimSpace(badOut), "a refused list prints nothing on stdout")
	}

	// Detail and session listings.
	detailOut, err := s.runCLIStdout("auth", "user", "get", user.ID.String(), "--json", server, s.caesiumURL)
	s.Require().NoError(err, detailOut)
	var detail adminUserDetailDoc
	s.Require().NoError(decodeCLIJSONStdout([]byte(detailOut), &detail), detailOut)
	s.Equal(int64(2), detail.SessionsTotal)
	s.Require().Len(detail.Sessions, 2)
	s.Equal([]string{"absolute_expires_at", "auth_method", "created_at", "id", "idle_expires_at", "last_seen_at",
		"source_ip", "user_agent", "user_id"}, jsonKeys(s, detail.Sessions[0]))
	s.NotContains(detailOut, sessA.TokenHash)
	s.NotContains(detailOut, sessA.CSRFToken)
	s.NotContains(detailOut, cookieA.Value)
	var detailIDs []string
	for _, raw := range detail.Sessions {
		var sess adminSessionDoc
		s.Require().NoError(json.Unmarshal(raw, &sess))
		detailIDs = append(detailIDs, sess.ID)
	}
	s.ElementsMatch([]string{sessA.ID.String(), sessB.ID.String()}, detailIDs)

	status, body, header = s.adminHTTP(http.MethodGet, "/v1/auth/sessions?user_id="+user.ID.String(), s.authAPIKey, nil, nil)
	s.Require().Equal(http.StatusOK, status, body)
	s.Equal("2", header.Get("X-Caesium-Total-Count"))
	status, body, header = s.adminHTTP(http.MethodGet, "/v1/auth/sessions?limit=1000", s.authAPIKey, nil, nil)
	s.Require().Equal(http.StatusOK, status, body)
	var everyone []adminSessionDoc
	s.Require().NoError(json.Unmarshal([]byte(body), &everyone), body)
	total, err := strconv.Atoi(header.Get("X-Caesium-Total-Count"))
	s.Require().NoError(err)
	s.GreaterOrEqual(total, 2, "the unfiltered listing spans every user")
	var everyoneIDs []string
	for _, sess := range everyone {
		everyoneIDs = append(everyoneIDs, sess.ID)
	}
	s.Subset(everyoneIDs, []string{sessA.ID.String(), sessB.ID.String()})

	// Disable: both sessions revoked in the same transaction; the very next
	// request on the cookie is refused.
	disabled := runCLIJSON[adminUserChangeDoc](s, "auth", "user", "disable", user.ID.String(), "--json", server, s.caesiumURL)
	s.True(disabled.Changed)
	s.Equal(int64(2), disabled.SessionsRevoked)
	s.NotNil(disabled.User.DisabledAt)
	status, body = s.requestWithCookie(http.MethodGet, "/auth/whoami", cookieA)
	s.Equal(http.StatusUnauthorized, status, "a disabled user's session is dead: %s", body)

	again := runCLIJSON[adminUserChangeDoc](s, "auth", "user", "disable", user.ID.String(), "--json", server, s.caesiumURL)
	s.False(again.Changed, "disabling a disabled user changes nothing")
	s.Zero(again.SessionsRevoked)

	s.Require().GreaterOrEqual(len(s.authAPIKey), 13)
	actor := s.authAPIKey[:13]
	disables := s.userAuditRows(iauth.ActionUserDisable, user.ID)
	s.Require().Len(disables, 1, "one user.disable row; the no-op repeat writes none")
	s.Equal(actor, disables[0].Actor)
	s.Equal("user", disables[0].ResourceType)
	s.Equal(models.ClusterNamespace, disables[0].Namespace)
	s.Equal(iauth.OutcomeSuccess, disables[0].Outcome)
	s.requireAuditMeta(disables[0], map[string]any{"sessions_revoked": float64(2)})
	revocations := s.userAuditRows(iauth.ActionSessionsRevoked, user.ID)
	s.Require().Len(revocations, 1)
	s.requireAuditMeta(revocations[0], map[string]any{"reason": "user_disabled", "count": float64(2)})

	// Enable: the revoked cookies stay dead.
	enabled := runCLIJSON[adminUserChangeDoc](s, "auth", "user", "enable", user.ID.String(), "--json", server, s.caesiumURL)
	s.True(enabled.Changed)
	s.Nil(enabled.User.DisabledAt)
	s.Len(s.userAuditRows(iauth.ActionUserEnable, user.ID), 1)
	status, body = s.requestWithCookie(http.MethodGet, "/auth/whoami", cookieB)
	s.Equal(http.StatusUnauthorized, status, "enable must not revive a revoked session: %s", body)

	// Log out everywhere: a fresh session is revoked.
	cookieC, _ := s.mintSession(f, user.ID)
	status, body = s.requestWithCookie(http.MethodGet, "/auth/whoami", cookieC)
	s.Require().Equal(http.StatusOK, status, "a session minted after the enable is live: %s", body)
	logout, err := s.runCLIStdout("auth", "user", "logout", user.ID.String(), "--json", server, s.caesiumURL)
	s.Require().NoError(err, logout)
	var out struct {
		UserID          string `json:"user_id"`
		SessionsRevoked int64  `json:"sessions_revoked"`
	}
	s.Require().NoError(decodeCLIJSONStdout([]byte(logout), &out), logout)
	s.Equal(user.ID.String(), out.UserID)
	s.Equal(int64(1), out.SessionsRevoked)
	status, body = s.requestWithCookie(http.MethodGet, "/auth/whoami", cookieC)
	s.Equal(http.StatusUnauthorized, status, "log out everywhere revokes the session: %s", body)
	revocations = s.userAuditRows(iauth.ActionSessionsRevoked, user.ID)
	s.Require().Len(revocations, 2)
	s.requireAuditMeta(revocations[0], map[string]any{"reason": "admin", "count": float64(1)})
}

// TestAuthUserAdministrationDeniesNonClusterAdmins proves every E3 route is
// cluster-admin only: an operator key fails the role check and a
// namespace-confined ADMIN key fails the cluster-class check, on all six
// routes, and nothing changes.
func (s *IntegrationTestSuite) TestAuthUserAdministrationDeniesNonClusterAdmins() {
	s.requireAuthLane()
	f := s.newUserAdminFixture("deny")
	target := s.seedAdminUser(f, "tess", time.Now().UTC())
	cookie, _ := s.mintSession(f, target.ID)

	operator := s.createAPIKeyCLI("--role", "operator", "--description", fmt.Sprintf("e3 operator %d", f.stamp))
	confined := s.createAPIKeyCLI("--role", "admin", "--description", fmt.Sprintf("e3 confined admin %d", f.stamp))
	s.Require().NoError(f.conn.Exec(`UPDATE api_keys SET namespaces = ? WHERE id = ?`, `["default"]`, confined.Meta.ID).Error)
	s.T().Cleanup(func() {
		for _, id := range []string{operator.Meta.ID, confined.Meta.ID} {
			out, err := s.runCLIStdout("auth", "key", "revoke", "--id", id, "--server", s.caesiumURL)
			s.NoError(err, "revoke %s: %s", id, out)
		}
	})

	for _, key := range []struct{ name, token string }{{"operator", operator.Plaintext}, {"confined admin", confined.Plaintext}} {
		for _, route := range userAdminRoutes {
			path := userAdminPath(route.path, target.ID)
			status, body, _ := s.adminHTTP(route.method, path, key.token, nil, nil)
			s.Equal(http.StatusForbidden, status, "%s on %s %s: %s", key.name, route.method, path, body)
		}
	}

	detail := runCLIJSON[adminUserDetailDoc](s, "auth", "user", "get", target.ID.String(), "--json", "--server", s.caesiumURL)
	s.Nil(detail.DisabledAt, "a denied disable changed nothing")
	s.Equal(int64(1), detail.SessionsTotal, "a denied revoke changed nothing")
	status, body := s.requestWithCookie(http.MethodGet, "/auth/whoami", cookie)
	s.Equal(http.StatusOK, status, body)
}

// TestAuthUserAdministrationCookieAdmin drives the routes as a cookie-session
// cluster admin: it appends a unique group bound to admin at "*" to the policy
// file the lane currently serves (restored afterwards), so the seeded user's
// grants resolve through the real policy source. Unsafe requests need the
// session's CSRF token, and an admin cannot disable its own user.
func (s *IntegrationTestSuite) TestAuthUserAdministrationCookieAdmin() {
	s.requireAuthLane()
	f := s.newUserAdminFixture("cookie")
	group := fmt.Sprintf("iaa-e3-admins-%d", f.stamp)

	path, original := s.authPolicyFixture()
	s.T().Cleanup(func() {
		s.restoreAuthPolicy(path, original)
		if _, _, err := s.runCLISeparate("auth", "policy", "reload", "--server", s.caesiumURL); err != nil {
			s.T().Errorf("restore the auth-lane policy revision: %v", err)
		}
	})
	var doc map[string]any
	s.Require().NoError(yaml.Unmarshal(original, &doc))
	bindings, _ := doc["bindings"].([]any)
	doc["bindings"] = append(bindings, map[string]any{
		"subjects":   map[string]any{"groups": []any{group}},
		"role":       "admin",
		"namespaces": []any{"*"},
	})
	updated, err := yaml.Marshal(doc)
	s.Require().NoError(err)
	s.writeAuthPolicy(path, updated)
	reload := runCLIJSON[policyReloadDoc](s, "auth", "policy", "reload", "--json", "--server", s.caesiumURL)
	s.Require().Equal(policy.Hash(updated), reload.Revision)

	now := time.Now().UTC()
	admin := s.seedAdminUser(f, "ada", now, group)
	target := s.seedAdminUser(f, "tom", now.Add(-time.Second))
	adminCookie, _ := s.mintSession(f, admin.ID)
	s.mintSession(f, target.ID)

	status, body := s.requestWithCookie(http.MethodGet, "/auth/whoami", adminCookie)
	s.Require().Equal(http.StatusOK, status, body)
	var who struct {
		Role      string `json:"role"`
		CSRFToken string `json:"csrf_token"`
	}
	s.Require().NoError(json.Unmarshal([]byte(body), &who), body)
	s.Require().Equal("admin", who.Role, "the appended binding makes the session a cluster admin")
	s.Require().NotEmpty(who.CSRFToken)

	status, body, _ = s.adminHTTP(http.MethodGet, "/v1/auth/users?issuer="+f.issuer, "", adminCookie, nil)
	s.Require().Equal(http.StatusOK, status, "a cookie cluster admin may list users: %s", body)

	disablePath := "/v1/auth/users/" + target.ID.String() + "/disable"
	status, body, _ = s.adminHTTP(http.MethodPost, disablePath, "", adminCookie, nil)
	s.Equal(http.StatusForbidden, status, "an unsafe cookie request without the CSRF token is refused: %s", body)
	detail := runCLIJSON[adminUserDetailDoc](s, "auth", "user", "get", target.ID.String(), "--json", "--server", s.caesiumURL)
	s.Nil(detail.DisabledAt, "the refused request changed nothing")

	csrf := map[string]string{"X-CSRF-Token": who.CSRFToken}
	status, body, _ = s.adminHTTP(http.MethodPost, disablePath, "", adminCookie, csrf)
	s.Require().Equal(http.StatusOK, status, body)
	var change adminUserChangeDoc
	s.Require().NoError(json.Unmarshal([]byte(body), &change), body)
	s.True(change.Changed)
	s.Equal(int64(1), change.SessionsRevoked)
	disables := s.userAuditRows(iauth.ActionUserDisable, target.ID)
	s.Require().Len(disables, 1)
	s.Equal(admin.Email, disables[0].Actor, "a cookie admin is audited by email")

	status, body, _ = s.adminHTTP(http.MethodPost, "/v1/auth/users/"+admin.ID.String()+"/disable", "", adminCookie, csrf)
	s.Equal(http.StatusConflict, status, "an admin cannot disable its own user: %s", body)
	status, body = s.requestWithCookie(http.MethodGet, "/auth/whoami", adminCookie)
	s.Equal(http.StatusOK, status, "the refused self-disable left the session alive: %s", body)
}

// TestUserAdministrationRoutesUnmountedWithoutAuth: with CAESIUM_AUTH_MODE=none
// the administration routes are not mounted at all, like the key routes. The
// name stays outside the auth lane's -run pattern.
func (s *IntegrationTestSuite) TestUserAdministrationRoutesUnmountedWithoutAuth() {
	s.skipOnAuthLane("the auth lane mounts the user administration routes")
	if strings.TrimSpace(os.Getenv("CAESIUM_AUTH_MODE")) == "api-key" {
		s.T().Skip("this runner targets an authenticated server")
	}
	id := uuid.New()
	for _, route := range userAdminRoutes {
		path := userAdminPath(route.path, id)
		req, err := http.NewRequestWithContext(s.T().Context(), route.method, s.caesiumURL+path, nil)
		s.Require().NoError(err)
		resp, err := http.DefaultClient.Do(req)
		s.Require().NoError(err)
		_ = resp.Body.Close()
		// No handler: a GET is a 404; a POST is a 405 because the embedded
		// UI's catch-all GET route matches the path.
		want := http.StatusNotFound
		if route.method == http.MethodPost {
			want = http.StatusMethodNotAllowed
		}
		s.Equal(want, resp.StatusCode, "%s %s must not be mounted without auth", route.method, path)
	}
}
