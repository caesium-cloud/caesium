//go:build integration

package test

import (
	"encoding/json"
	"net/http"
	"time"

	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// User-bound API keys (identity-and-access E4). No route mints one until E5's
// `caesium login`, so each scenario produces its key the way E3's scenarios
// produce sessions: the real iauth.Service.MintUserBoundKey (with its user-row
// fence) runs against the live catalog with the lane's hash secret. That is
// the precondition only; every assertion goes through the server, over HTTP
// or the caesium CLI.

type userKeyWhoami struct {
	Kind       string            `json:"kind"`
	Subject    string            `json:"subject"`
	Email      string            `json:"email"`
	StableID   string            `json:"stable_id"`
	Grants     map[string]string `json:"grants"`
	Namespaces []string          `json:"namespaces"`
	CSRFToken  string            `json:"csrf_token"`
}

type userKeyDoc struct {
	ID          string     `json:"id"`
	KeyPrefix   string     `json:"key_prefix"`
	Description string     `json:"description"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
	LastUsedAt  *time.Time `json:"last_used_at"`
}

type userKeysDetailDoc struct {
	ID            string            `json:"id"`
	UserKeys      []json.RawMessage `json:"user_keys"`
	UserKeysTotal int64             `json:"user_keys_total"`
}

type userKeysChangeDoc struct {
	Changed         bool  `json:"changed"`
	SessionsRevoked int64 `json:"sessions_revoked"`
	KeysRevoked     int64 `json:"keys_revoked"`
}

type userKeysRevokeDoc struct {
	UserID          string `json:"user_id"`
	SessionsRevoked int64  `json:"sessions_revoked"`
	KeysRevoked     int64  `json:"keys_revoked"`
}

// userKeyMinter mints user-bound keys for one E3 fixture's users and deletes
// them when the scenario ends (before the fixture deletes the users).
type userKeyMinter struct {
	s   *IntegrationTestSuite
	f   *userAdminFixture
	svc *iauth.Service
}

func (s *IntegrationTestSuite) newUserKeyMinter(f *userAdminFixture, opts ...iauth.ServiceOption) *userKeyMinter {
	s.T().Helper()
	vars := env.Variables()
	s.Require().NotEmpty(vars.AuthKeyHashSecret, "the auth lane runner must carry CAESIUM_AUTH_KEY_HASH_SECRET")
	m := &userKeyMinter{
		s:   s,
		f:   f,
		svc: iauth.NewService(f.conn, append([]iauth.ServiceOption{iauth.WithKeyHashSecret(vars.AuthKeyHashSecret)}, opts...)...),
	}
	// Registered after the fixture's cleanup, so it runs first.
	s.T().Cleanup(func() {
		s.NoError(f.conn.Exec(`DELETE FROM api_keys WHERE user_id IN (SELECT id FROM users WHERE issuer = ?)`, f.issuer).Error)
	})
	return m
}

func (m *userKeyMinter) mint(userID uuid.UUID) *iauth.CreateKeyResponse {
	m.s.T().Helper()
	var row models.User
	m.s.Require().NoError(m.f.conn.Where("id = ?", userID).Take(&row).Error)
	resp, err := m.svc.MintUserBoundKey(m.s.T().Context(), iauth.MintUserKeyRequest{
		UserID:            userID,
		DisableGeneration: row.DisableGeneration,
		Description:       "integration e4",
		CreatedBy:         row.Email,
	})
	m.s.Require().NoError(err)
	return resp
}

func (s *IntegrationTestSuite) userKeyWhoami(token string) (int, userKeyWhoami) {
	s.T().Helper()
	status, body := s.requestWithKey(http.MethodGet, "/auth/whoami", token, nil)
	var who userKeyWhoami
	if status == http.StatusOK {
		s.Require().NoError(json.Unmarshal([]byte(body), &who), body)
	}
	return status, who
}

// authFailureWindow is the server's per-IP authentication-failure window
// (cmd/start: auth.NewRateLimiter(CAESIUM_AUTH_RATE_LIMIT_PER_MINUTE,
// time.Minute)). The lanes keep the default of 10 failures a window, and every
// 401 a scenario provokes counts against the runner's one IP, shared with
// every other scenario in the suite.
const authFailureWindow = time.Minute

// TestAuthUserBoundKeyLifecycle drives user-bound keys through their whole
// life on any api-key auth server, with or without a policy file (it is the
// coverage journey's E4 scenario; its users match no binding, so their keys
// authenticate with no grants): a key is its user (whoami), RBAC refuses it
// 403 (not 401), it is listed under the user and in the cluster admin's key
// list, it is never rotated, and it dies on admin revoke, on disable, on log
// out everywhere, on a disable the cascade never saw, and at its expiry.
//
// Every refusal is a counted authentication failure, so the scenario puts
// each key in its final state first and asserts the five 401s together, in a
// failure window of their own: it waits until a full window has passed since
// it started (every earlier scenario's failures have then expired) and
// provokes no failure before. It so neither trips the limiter on top of
// earlier scenarios nor spends more than five failures of a later one's
// window.
func (s *IntegrationTestSuite) TestAuthUserBoundKeyLifecycle() {
	started := time.Now()
	s.requireAuthLane()
	f := s.newUserAdminFixture("e4-life")
	m := s.newUserKeyMinter(f)
	now := time.Now().UTC().Truncate(time.Second)
	user := s.seedAdminUser(f, "ulf", now)
	id := user.ID.String()
	server := "--server"

	key1 := m.mint(user.ID)
	s.Require().Contains(key1.Plaintext, iauth.KeyPrefixUser)

	status, who := s.userKeyWhoami(key1.Plaintext)
	s.Require().Equal(http.StatusOK, status)
	s.Equal("user", who.Kind)
	s.Equal(user.Email, who.Subject, "the principal's subject is the user's email, not the key prefix")
	s.Equal(user.Email, who.Email)
	s.Equal(id, who.StableID)
	s.Empty(who.Grants)
	s.Empty(who.Namespaces)
	s.Empty(who.CSRFToken, "a bearer key carries no CSRF token")

	status, body := s.requestWithKey(http.MethodGet, "/v1/atoms", key1.Plaintext, nil)
	s.Equal(http.StatusForbidden, status, "a grant-less user's key authenticates and is refused by RBAC: %s", body)

	// Listed under the user (REST and CLI --json) with no credential material.
	status, body = s.requestWithKey(http.MethodGet, "/v1/auth/users/"+id, s.authAPIKey, nil)
	s.Require().Equal(http.StatusOK, status, body)
	var detail userKeysDetailDoc
	s.Require().NoError(json.Unmarshal([]byte(body), &detail), body)
	s.Require().Equal(int64(1), detail.UserKeysTotal)
	s.Require().Len(detail.UserKeys, 1)
	s.Equal([]string{"created_at", "description", "expires_at", "id", "key_prefix", "last_used_at"}, jsonKeys(s, detail.UserKeys[0]))
	var listed userKeyDoc
	s.Require().NoError(json.Unmarshal(detail.UserKeys[0], &listed))
	s.Equal(key1.Key.ID.String(), listed.ID)
	s.Equal(key1.Key.KeyPrefix, listed.KeyPrefix)
	s.Require().NotNil(listed.ExpiresAt)
	s.WithinDuration(key1.Key.ExpiresAt.UTC(), listed.ExpiresAt.UTC(), time.Second)
	s.NotContains(body, key1.Key.KeyHash)
	s.NotContains(body, key1.Plaintext)

	cliDetail := runCLIJSON[userKeysDetailDoc](s, "auth", "user", "get", id, "--json", server, s.caesiumURL)
	s.Equal(id, cliDetail.ID)
	s.Require().Len(cliDetail.UserKeys, 1)
	s.Contains(string(cliDetail.UserKeys[0]), key1.Key.ID.String())

	// The cluster admin's key list shows it with its user and no hash.
	keys := runCLIJSON[[]map[string]any](s, "auth", "key", "list", server, s.caesiumURL)
	var found map[string]any
	for _, k := range keys {
		if k["id"] == key1.Key.ID.String() {
			found = k
		}
	}
	s.Require().NotNil(found, "the cluster admin's key list shows the user-bound key")
	s.Equal(id, found["user_id"])
	s.NotContains(found, "key_hash")

	// Never rotated: a login mints a new one.
	status, body = s.requestWithKey(http.MethodPost, "/v1/auth/keys/"+key1.Key.ID.String()+"/rotate", s.authAPIKey, nil)
	s.Equal(http.StatusConflict, status, body)
	s.Contains(body, "user-bound")

	// key1: revoked by a cluster admin.
	out, err := s.runCLIStdout("auth", "key", "revoke", "--id", key1.Key.ID.String(), server, s.caesiumURL)
	s.Require().NoError(err, out)

	// key2: revoked by disabling its user, in the same transaction as the
	// user's sessions.
	key2 := m.mint(user.ID)
	disabled := runCLIJSON[userKeysChangeDoc](s, "auth", "user", "disable", id, "--json", server, s.caesiumURL)
	s.True(disabled.Changed)
	s.Equal(int64(1), disabled.KeysRevoked, "key2 is the user's only unrevoked key")
	disables := s.userAuditRows(iauth.ActionUserDisable, user.ID)
	s.Require().Len(disables, 1)
	s.requireAuditMeta(disables[0], map[string]any{"sessions_revoked": float64(0), "keys_revoked": float64(1)})
	revocations := s.userAuditRows(iauth.ActionSessionsRevoked, user.ID)
	s.Require().Len(revocations, 1)
	s.requireAuditMeta(revocations[0], map[string]any{"reason": "user_disabled", "count": float64(0), "keys_revoked": float64(1)})

	// key3: minted after the enable, revoked by logging the user out
	// everywhere.
	_ = runCLIJSON[userKeysChangeDoc](s, "auth", "user", "enable", id, "--json", server, s.caesiumURL)
	key3 := m.mint(user.ID)
	status, _ = s.userKeyWhoami(key3.Plaintext)
	s.Require().Equal(http.StatusOK, status, "a key minted after the enable is live")
	logout := runCLIJSON[userKeysRevokeDoc](s, "auth", "user", "logout", id, "--json", server, s.caesiumURL)
	s.Equal(id, logout.UserID)
	s.Equal(int64(1), logout.KeysRevoked)
	after := runCLIJSON[userKeysDetailDoc](s, "auth", "user", "get", id, "--json", server, s.caesiumURL)
	s.Zero(after.UserKeysTotal, "no key of the user is active any more")

	// keyU: its user is disabled in the catalog without the cascade, a
	// precondition no surface can produce; the refusal below is the server's.
	other := s.seedAdminUser(f, "una", now)
	keyU := m.mint(other.ID)
	status, _ = s.userKeyWhoami(keyU.Plaintext)
	s.Require().Equal(http.StatusOK, status)
	s.Require().NoError(f.conn.Model(&models.User{}).Where("id = ?", other.ID).Update("disabled_at", time.Now().UTC()).Error)

	// keyX: a key of a third user that expires seconds after its mint.
	short := s.newUserKeyMinter(f, iauth.WithUserKeyTTL(3*time.Second))
	expiring := short.mint(s.seedAdminUser(f, "uxi", now).ID)
	status, _ = s.userKeyWhoami(expiring.Plaintext)
	s.Require().Equal(http.StatusOK, status)

	// The five refusals, in a failure window of their own.
	time.Sleep(time.Until(started.Add(authFailureWindow + time.Second)))
	if wait := time.Until(expiring.Key.ExpiresAt.Add(time.Second)); wait > 0 {
		time.Sleep(wait)
	}
	for _, refused := range []struct {
		key    *iauth.CreateKeyResponse
		reason string
	}{
		{key1, "an admin-revoked user-bound key is dead"},
		{key2, "disabling the user kills its key"},
		{key3, "logging the user out everywhere kills its key"},
		{keyU, "validation checks the user, not only the revocation cascade"},
		{expiring, "an expired user-bound key is dead"},
	} {
		status, _ = s.userKeyWhoami(refused.key.Plaintext)
		s.Equal(http.StatusUnauthorized, status, refused.reason)
	}
}

// TestAuthUserBoundKeyGrantsFollowPolicy needs the lane's policy file
// (marketing-eng is operator in marketing, finance-eng in finance): a
// user-bound key holds exactly its user's grants, resolved from the stored
// groups on every request, and a namespace admin never sees it in its key
// list.
func (s *IntegrationTestSuite) TestAuthUserBoundKeyGrantsFollowPolicy() {
	s.requireAuthLane()
	s.requirePolicyFixture()
	f := s.newUserAdminFixture("e4-grants")
	m := s.newUserKeyMinter(f)
	user := s.seedAdminUser(f, "mae", time.Now().UTC().Truncate(time.Second), "marketing-eng")
	ns := s.newNamespacedRoutesFixture("e4")
	defer s.closeNamespacedRoutesFixture(ns)
	key := m.mint(user.ID)

	requireJob := func(want int, job *jobSummary) {
		s.T().Helper()
		status, body := s.requestWithKey(http.MethodGet, "/v1/jobs/"+job.ID, key.Plaintext, nil)
		s.Require().Equal(want, status, "GET job %s: %s", job.Alias, body)
	}
	requireJob(http.StatusOK, ns.mktJob)
	requireJob(http.StatusForbidden, ns.finJob)
	status, who := s.userKeyWhoami(key.Plaintext)
	s.Require().Equal(http.StatusOK, status)
	s.Equal("user", who.Kind)
	s.Equal(user.Email, who.Subject)
	s.Equal(user.ID.String(), who.StableID)
	s.Equal(map[string]string{"marketing": "operator"}, who.Grants)
	s.Equal([]string{"marketing"}, who.Namespaces)

	// The user's stored groups change (as a login or E2's refresh would
	// write them): the same key follows on its next request.
	s.Require().NoError(f.conn.Model(&models.User{}).Where("id = ?", user.ID).Update("groups", datatypes.JSON(`["finance-eng"]`)).Error)
	requireJob(http.StatusForbidden, ns.mktJob)
	requireJob(http.StatusOK, ns.finJob)
	_, who = s.userKeyWhoami(key.Plaintext)
	s.Equal(map[string]string{"finance": "operator"}, who.Grants)

	// A marketing admin's key list never shows a user-bound key.
	mktAdmin := s.mintNamespacedKey("admin", "marketing")
	status, body := s.requestWithKey(http.MethodGet, "/v1/auth/keys", mktAdmin.Plaintext, nil)
	s.Require().Equal(http.StatusOK, status, body)
	var visible []map[string]any
	s.Require().NoError(json.Unmarshal([]byte(body), &visible), body)
	for _, k := range visible {
		s.NotEqual(key.Key.ID.String(), k["id"], "a namespace admin never sees a user-bound key")
		s.NotContains(k, "user_id")
	}
}
