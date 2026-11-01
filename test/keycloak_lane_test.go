//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/caesium-cloud/caesium/internal/models"
)

// The Keycloak lane's SSO scenarios (plan item G1, design §12.2). Each one
// logs a realm user in through Keycloak's real HTML login form and asserts
// what the session may do, against the lane policy
// test/testdata/keycloak/policy.yaml. G3 extends them with the full allow/deny
// matrix, group refresh and user disable.

// TestKeycloakSSOAdminLogin: admin@example.com, in caesium-admins, holds admin
// in every namespace and sees both lane jobs.
func (s *IntegrationTestSuite) TestKeycloakSSOAdminLogin() {
	lane := s.requireKeycloakLane()
	login := s.keycloakOIDCLogin(lane, "admin@example.com")
	who := s.requireSSOSession(login)

	s.Equal("admin@example.com", who.Subject)
	s.Equal("admin@example.com", who.Email)
	s.Equal("admin", who.Role)
	s.Equal("admin", who.ClusterRole)
	s.Equal(map[string]string{"*": "admin"}, who.Grants)
	s.Equal([]string{"*"}, who.Namespaces)

	aliases := s.keycloakJobAliases(login)
	s.Contains(aliases, keycloakMarketingJob)
	s.Contains(aliases, keycloakFinanceJob)
}

// TestKeycloakSSOConfinedLogin: bob@example.com, in marketing-eng, is an
// operator in marketing only: the finance job is absent from his list and
// refused by id.
func (s *IntegrationTestSuite) TestKeycloakSSOConfinedLogin() {
	lane := s.requireKeycloakLane()
	marketing := s.requireJobByAlias(keycloakMarketingJob)
	finance := s.requireJobByAlias(keycloakFinanceJob)

	login := s.keycloakOIDCLogin(lane, "bob@example.com")
	who := s.requireSSOSession(login)
	s.Equal("bob@example.com", who.Subject)
	s.Empty(who.Role, "a confined principal has no cluster role")
	s.Empty(who.ClusterRole)
	s.Equal(map[string]string{"marketing": "operator"}, who.Grants)
	s.Equal([]string{"marketing"}, who.Namespaces)

	aliases := s.keycloakJobAliases(login)
	s.Contains(aliases, keycloakMarketingJob)
	s.NotContains(aliases, keycloakFinanceJob)

	status, body := login.get(s, "/v1/jobs/"+marketing.ID)
	s.Equal(http.StatusOK, status, "bob reads the marketing job: %s", body)
	status, body = login.get(s, "/v1/jobs/"+finance.ID)
	s.Equal(http.StatusForbidden, status, "bob is refused the finance job: %s", body)
}

// TestKeycloakSSONoBindingDenied: carol@example.com is in no group and matches
// no binding, so her login is denied with reason no_binding and no session.
func (s *IntegrationTestSuite) TestKeycloakSSONoBindingDenied() {
	lane := s.requireKeycloakLane()
	since := time.Now().Add(-time.Minute)
	login := s.keycloakOIDCLogin(lane, "carol@example.com")
	s.requireNoBindingCallback(login)
	s.requireLoginDeniedAudit("carol@example.com", "no_binding", since)
}

// TestKeycloakSSOUnverifiedEmailDenied: dave@example.com is in no group and
// his email is bound to admin in every namespace, but Keycloak reports it
// unverified, so Caesium must not trust it (design §6.3: only OIDC
// email_verified=true email reaches users: bindings). The audit row's actor is
// his email, which proves the claim reached Caesium and was refused, not lost.
func (s *IntegrationTestSuite) TestKeycloakSSOUnverifiedEmailDenied() {
	lane := s.requireKeycloakLane()
	since := time.Now().Add(-time.Minute)
	login := s.keycloakOIDCLogin(lane, "dave@example.com")
	s.requireNoBindingCallback(login)
	s.requireLoginDeniedAudit("dave@example.com", "no_binding", since)
}

// TestKeycloakSSOVerifiedEmailBinding is dave's positive control:
// erin@example.com is in no group either, and her VERIFIED email is bound to
// viewer in finance, so the email binding alone grants exactly that.
func (s *IntegrationTestSuite) TestKeycloakSSOVerifiedEmailBinding() {
	lane := s.requireKeycloakLane()
	marketing := s.requireJobByAlias(keycloakMarketingJob)
	finance := s.requireJobByAlias(keycloakFinanceJob)

	login := s.keycloakOIDCLogin(lane, "erin@example.com")
	who := s.requireSSOSession(login)
	s.Equal("erin@example.com", who.Subject)
	s.Empty(who.ClusterRole)
	s.Equal(map[string]string{"finance": "viewer"}, who.Grants)
	s.Equal([]string{"finance"}, who.Namespaces)

	aliases := s.keycloakJobAliases(login)
	s.Contains(aliases, keycloakFinanceJob)
	s.NotContains(aliases, keycloakMarketingJob)
	status, body := login.get(s, "/v1/jobs/"+finance.ID)
	s.Equal(http.StatusOK, status, "erin reads the finance job: %s", body)
	status, body = login.get(s, "/v1/jobs/"+marketing.ID)
	s.Equal(http.StatusForbidden, status, "erin is refused the marketing job: %s", body)
}

// ---- G3 + G4 (identity-and-access W8-β, design §11 and §13): the identity
// lifecycle with real Keycloak users. Every scenario that changes a user's
// groups, sessions or state creates its own realm user (keycloakAdmin), so
// the fixture users stay as G1 and the browser journeys expect them.

var (
	keycloakMarketingOperator = map[string]string{"marketing": "operator"}
	keycloakFinanceOperator   = map[string]string{"finance": "operator"}
)

// keycloakGroupGrants is what the lane policy grants marketing-eng and
// finance-eng.
func keycloakGroupGrants(group string) map[string]string {
	if group == "finance-eng" {
		return keycloakFinanceOperator
	}
	return keycloakMarketingOperator
}

func keycloakOtherGroup(group string) string {
	if group == "finance-eng" {
		return "marketing-eng"
	}
	return "finance-eng"
}

// sessionGrantsAre reports whether the browser session is admitted with
// exactly want.
func (s *IntegrationTestSuite) sessionGrantsAre(login *keycloakLogin, want map[string]string) bool {
	status, who, _ := login.whoami(s)
	return status == http.StatusOK && reflect.DeepEqual(who.Grants, want)
}

// cliGrantsAre reports whether `caesium whoami --json` with the stored key
// shows exactly want.
func (s *IntegrationTestSuite) cliGrantsAre(cli *keycloakCLI, want map[string]string) bool {
	who, _, err := cli.whoami(s)
	return err == nil && reflect.DeepEqual(who.Grants, want)
}

// staleRefusal asserts a 401 that names stale_groups (the one 401 that says
// why, so the Console and the CLI can ask for a new sign-in).
func (s *IntegrationTestSuite) requireStaleRefusal(res cliHTTPResult, what string) {
	s.T().Helper()
	s.Require().Equal(http.StatusUnauthorized, res.status, "%s: %s", what, res.body)
	var refusal map[string]string
	s.Require().NoError(json.Unmarshal([]byte(res.body), &refusal), "%s: %s", what, res.body)
	s.Equal("stale_groups", refusal["reason"], what)
}

// TestKeycloakSSOAdminMintsNamespacedKey (AC1, A5): the SSO admin mints a key
// confined to marketing from its Console session (cookie + CSRF), and the key
// holds exactly that grant; a confined SSO operator may not mint at all.
func (s *IntegrationTestSuite) TestKeycloakSSOAdminMintsNamespacedKey() {
	lane := s.requireKeycloakLane()
	marketing := s.requireJobByAlias(keycloakMarketingJob)
	finance := s.requireJobByAlias(keycloakFinanceJob)

	admin := s.keycloakOIDCLogin(lane, "admin@example.com")
	s.requireSSOSession(admin)
	desc := fmt.Sprintf("g3 sso admin mint %d", time.Now().UnixNano())
	status, body := admin.send(s, http.MethodPost, "/v1/auth/keys", `{"description":"`+desc+`","role":"operator","namespaces":["marketing"]}`)
	s.Require().Equal(http.StatusCreated, status, "the SSO admin mints a namespaced key: %s", body)
	key := s.parseCreatedKey(string(body))
	s.T().Cleanup(func() {
		if out, err := s.runCLIStdout("auth", "key", "revoke", "--id", key.Meta.ID, "--server", s.caesiumURL); err != nil {
			s.T().Errorf("revoke the SSO-minted key: %v\n%s", err, out)
		}
	})
	s.Equal("operator", key.Meta.Role)
	s.Equal([]string{"marketing"}, key.Meta.Namespaces)
	s.Equal("admin@example.com", key.Meta.CreatedBy, "the mint is attributed to the SSO admin")

	status, whoBody := s.requestWithKey(http.MethodGet, "/auth/whoami", key.Plaintext, nil)
	s.Require().Equal(http.StatusOK, status, whoBody)
	var who keycloakWhoami
	s.Require().NoError(json.Unmarshal([]byte(whoBody), &who), whoBody)
	s.Equal(keycloakMarketingOperator, who.Grants)
	status, out := s.requestWithKey(http.MethodGet, "/v1/jobs/"+marketing.ID, key.Plaintext, nil)
	s.Equal(http.StatusOK, status, "the minted key reads the marketing job: %s", out)
	status, out = s.requestWithKey(http.MethodGet, "/v1/jobs/"+finance.ID, key.Plaintext, nil)
	s.Equal(http.StatusForbidden, status, "the minted key is refused the finance job: %s", out)

	bob := s.keycloakOIDCLogin(lane, "bob@example.com")
	s.requireSSOSession(bob)
	status, body = bob.send(s, http.MethodPost, "/v1/auth/keys", `{"description":"`+desc+` bob","role":"viewer","namespaces":["marketing"]}`)
	s.Equal(http.StatusForbidden, status, "an SSO operator administers no key: %s", body)

	// An SSO admin confined to marketing mints only within its grants.
	confined := s.keycloakMatrixPrincipal(lane, s.keycloakAdmin(lane), "admin", "marketing")
	status, body = confined.Login.send(s, http.MethodPost, "/v1/auth/keys", `{"description":"`+desc+` confined","role":"viewer","namespaces":["marketing"]}`)
	s.Require().Equal(http.StatusCreated, status, "a marketing admin mints a marketing key: %s", body)
	inside := s.parseCreatedKey(string(body))
	s.T().Cleanup(func() {
		if out, err := s.runCLIStdout("auth", "key", "revoke", "--id", inside.Meta.ID, "--server", s.caesiumURL); err != nil {
			s.T().Errorf("revoke the confined SSO admin's key: %v\n%s", err, out)
		}
	})
	s.Equal([]string{"marketing"}, inside.Meta.Namespaces)
	status, body = confined.Login.send(s, http.MethodPost, "/v1/auth/keys", `{"description":"`+desc+` confined finance","role":"viewer","namespaces":["finance"]}`)
	s.Equal(http.StatusForbidden, status, "a marketing admin may not mint for finance: %s", body)
	status, body = confined.Login.send(s, http.MethodPost, "/v1/auth/keys", `{"description":"`+desc+` confined cluster","role":"viewer"}`)
	s.Equal(http.StatusForbidden, status, "a marketing admin may not mint a cluster-wide key: %s", body)
}

// TestKeycloakGroupRefreshBrowserSession (AC5): a group change in Keycloak
// reaches a browser session without a new sign-in. Within the interval the
// stored groups stand; past it the next request queues a refresh through the
// user's refresh token, after which the new grants apply.
func (s *IntegrationTestSuite) TestKeycloakGroupRefreshBrowserSession() {
	lane := s.requireKeycloakLane()
	kc := s.keycloakAdmin(lane)
	user := kc.createUser("refresh-browser", "marketing-eng")
	login := s.keycloakOIDCLogin(lane, user.Username)
	loggedIn := time.Now()
	who := s.requireSSOSession(login)
	s.Require().Equal(keycloakMarketingOperator, who.Grants)

	kc.move(user, "marketing-eng", "finance-eng")
	s.True(s.sessionGrantsAre(login, keycloakMarketingOperator), "within the interval the stored groups stand")

	keycloakPastInterval(lane, loggedIn)
	s.keycloakAwait(lane, "the browser session sees the refreshed groups without a new sign-in", func() bool {
		return s.sessionGrantsAre(login, keycloakFinanceOperator)
	})
	aliases := s.keycloakJobAliases(login)
	s.Contains(aliases, keycloakFinanceJob)
	s.NotContains(aliases, keycloakMarketingJob)

	audits := s.awaitUserAuditRows("auth.groups_refreshed", uuid.MustParse(who.StableID), 1)
	s.Equal("system:group-refresh", audits[0].Actor)
}

// TestKeycloakGroupRefreshCLIOnlyAndRotation (AC5): a user who signed in once
// for the CLI and then only presents its user-bound key gets the new grants
// without signing in again; then refreshes alternate between that browser
// session and the key. The realm revokes a refresh token on use, so a refresh
// that redeemed a superseded token would be refused with invalid_grant and
// revoke the user's credentials: every round passing, with no revocation,
// proves each rotated token was persisted for the user, whichever credential
// drove the refresh.
func (s *IntegrationTestSuite) TestKeycloakGroupRefreshCLIOnlyAndRotation() {
	lane := s.requireKeycloakLane()
	kc := s.keycloakAdmin(lane)
	status, body, _ := kc.call(http.MethodGet, "", nil)
	s.Require().Equal(http.StatusOK, status, "Keycloak realm representation")
	var realm struct {
		RevokeRefreshToken   bool `json:"revokeRefreshToken"`
		RefreshTokenMaxReuse int  `json:"refreshTokenMaxReuse"`
	}
	s.Require().NoError(json.Unmarshal(body, &realm))
	s.Require().True(realm.RevokeRefreshToken, "the live realm must revoke refresh tokens on use, or rotation proves nothing")
	s.Require().Zero(realm.RefreshTokenMaxReuse)
	user := kc.createUser("refresh-cli", "marketing-eng")
	cli := s.keycloakCLILogin(lane, user.Username, false)
	refreshed := time.Now()
	who, stderr, err := cli.whoami(s)
	s.Require().NoError(err, stderr)
	s.Equal("user", who.Kind)
	s.Equal(user.Username, who.Subject)
	s.Require().Equal(keycloakMarketingOperator, who.Grants)
	userID := uuid.MustParse(who.StableID)

	// CLI only: the key alone drives the refresh.
	group := "finance-eng"
	kc.move(user, "marketing-eng", group)
	keycloakPastInterval(lane, refreshed)
	s.keycloakAwait(lane, "the CLI-only user's key sees the refreshed groups", func() bool {
		return s.cliGrantsAre(cli, keycloakGroupGrants(group))
	})
	refreshed = time.Now()

	for round := 1; round <= 3; round++ {
		next := keycloakOtherGroup(group)
		kc.move(user, group, next)
		group = next
		want := keycloakGroupGrants(group)
		keycloakPastInterval(lane, refreshed)
		if round%2 == 1 {
			s.keycloakAwait(lane, fmt.Sprintf("round %d: the browser session drives the refresh", round), func() bool {
				return s.sessionGrantsAre(cli.browser, want)
			})
			s.True(s.cliGrantsAre(cli, want), "round %d: the key shares the user's refreshed groups", round)
		} else {
			s.keycloakAwait(lane, fmt.Sprintf("round %d: the key drives the refresh", round), func() bool {
				return s.cliGrantsAre(cli, want)
			})
			s.True(s.sessionGrantsAre(cli.browser, want), "round %d: the session shares the user's refreshed groups", round)
		}
		refreshed = time.Now()
	}

	s.Len(s.awaitUserAuditRows("auth.groups_refreshed", userID, 4), 4, "one audited change per refresh")
	s.Empty(s.userAuditRows("auth.sessions_revoked", userID), "no refresh was refused")
}

// TestKeycloakStaleGroupsAfterIdPOutage (AC5): while the IdP refuses the
// client (an OAuth error other than invalid_grant: an outage, not a
// revocation), a user whose groups pass the maximum staleness is refused with
// 401 stale_groups. The refusal is persisted for the user: after the IdP
// recovers, the session and the existing user-bound key both stay refused
// until a new sign-in, which lifts it for every credential.
func (s *IntegrationTestSuite) TestKeycloakStaleGroupsAfterIdPOutage() {
	lane := s.requireKeycloakLane()
	kc := s.keycloakAdmin(lane)
	user := kc.createUser("stale", "marketing-eng")
	cli := s.keycloakCLILogin(lane, user.Username, false)
	refreshed := time.Now()
	cookie := cli.browser.sessionCookie(s)
	s.Require().True(s.cliGrantsAre(cli, keycloakMarketingOperator))

	kc.setClientEnabled(false)
	// No request until the groups pass the bound, so nothing refreshes (or
	// fails to) earlier: the first request past it waits for one
	// synchronous refresh, which the IdP refuses.
	time.Sleep(time.Until(refreshed.Add(lane.maxStaleness + 2*time.Second)))
	keycloakBudgetFailures(3)
	s.requireStaleRefusal(s.cliHTTP(http.MethodGet, "/auth/whoami", "", cookie, "", ""), "past the bound with the IdP down")

	kc.setClientEnabled(true)
	s.requireStaleRefusal(s.cliHTTP(http.MethodGet, "/auth/whoami", "", cookie, "", ""), "the session after the IdP recovered")
	s.requireStaleRefusal(s.cliHTTP(http.MethodGet, "/auth/whoami", cli.key, nil, "", ""), "the user-bound key after the IdP recovered")

	fresh := s.keycloakOIDCLogin(lane, user.Username)
	s.Equal(keycloakMarketingOperator, s.requireSSOSession(fresh).Grants, "a new sign-in is admitted")
	s.True(s.sessionGrantsAre(cli.browser, keycloakMarketingOperator), "the new sign-in lifts the refusal for the earlier session")
	s.True(s.cliGrantsAre(cli, keycloakMarketingOperator), "and for the user-bound key")
}

// TestKeycloakUserDisableKillsCredentials (AC5, E3 with a real SSO user): the
// cluster admin finds the user with `caesium auth user list --json` (stdout
// alone is the JSON array), sees its session and user-bound key with `get`,
// and `disable` kills both within one request: the very next request of
// each is refused. `enable` lets the user sign in again.
func (s *IntegrationTestSuite) TestKeycloakUserDisableKillsCredentials() {
	lane := s.requireKeycloakLane()
	kc := s.keycloakAdmin(lane)
	user := kc.createUser("disable", "marketing-eng")
	cli := s.keycloakCLILogin(lane, user.Username, false)
	cookie := cli.browser.sessionCookie(s)
	who, stderr, err := cli.whoami(s)
	s.Require().NoError(err, stderr)
	userID := who.StableID

	stdout, stderr, err := s.runCLISeparate("auth", "user", "list", "--json", "--email", user.Username, "--server", s.caesiumURL)
	s.Require().NoError(err, stderr)
	var users []struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	s.Require().NoError(json.Unmarshal([]byte(stdout), &users), "auth user list --json stdout must be exactly the JSON array: %q", stdout)
	s.Require().Len(users, 1, stdout)
	s.Equal(user.Username, users[0].Email)
	s.Require().Equal(userID, users[0].ID)

	stdout, stderr, err = s.runCLISeparate("auth", "user", "get", userID, "--json", "--server", s.caesiumURL)
	s.Require().NoError(err, stderr)
	var detail struct {
		Email         string `json:"email"`
		SessionsTotal int64  `json:"sessions_total"`
		UserKeysTotal int64  `json:"user_keys_total"`
	}
	s.Require().NoError(json.Unmarshal([]byte(stdout), &detail), "auth user get --json stdout: %q", stdout)
	s.Equal(user.Username, detail.Email)
	s.EqualValues(1, detail.SessionsTotal, "the CLI login's browser session")
	s.EqualValues(1, detail.UserKeysTotal, "the CLI login's user-bound key")

	stdout, stderr, err = s.runCLISeparate("auth", "user", "disable", userID, "--json", "--server", s.caesiumURL)
	s.Require().NoError(err, stderr)
	var change struct {
		Changed         bool  `json:"changed"`
		SessionsRevoked int64 `json:"sessions_revoked"`
		KeysRevoked     int64 `json:"keys_revoked"`
	}
	s.Require().NoError(json.Unmarshal([]byte(stdout), &change), "auth user disable --json stdout: %q", stdout)
	s.True(change.Changed)
	s.EqualValues(1, change.SessionsRevoked)
	s.EqualValues(1, change.KeysRevoked)

	keycloakBudgetFailures(3)
	res := s.cliHTTP(http.MethodGet, "/auth/whoami", "", cookie, "", "")
	s.Equal(http.StatusUnauthorized, res.status, "the session's very next request is refused: %s", res.body)
	s.requireCLIRefused(cli, "the user-bound key's very next request")
	s.Len(s.awaitUserAuditRows("user.disable", uuid.MustParse(userID), 1), 1)

	// G4: `caesium logout` with the dead key still removes the stored
	// credential, and says the server did not revoke it.
	_, stderr, err = s.runCLIWithEnv(cli.env, "logout", "--server", s.caesiumURL)
	s.Require().Error(err, "logout with a refused key reports it")
	s.Contains(stderr, "did not revoke it")
	s.Contains(stderr, "logout failed (401)")
	s.NotContains(stderr, cli.key)
	s.requireCredentialRemoved(cli)

	stdout, stderr, err = s.runCLISeparate("auth", "user", "enable", userID, "--json", "--server", s.caesiumURL)
	s.Require().NoError(err, stderr)
	s.Require().NoError(json.Unmarshal([]byte(stdout), &change), "auth user enable --json stdout: %q", stdout)
	s.True(change.Changed)
	fresh := s.keycloakOIDCLogin(lane, user.Username)
	s.Equal(keycloakMarketingOperator, s.requireSSOSession(fresh).Grants, "an enabled user signs in again")
}

// TestKeycloakCLILoginRoundTrip (G4, AC5): `caesium login --no-browser`
// through Keycloak (the signed-out finish page offers the OIDC sign-in), then
// `caesium whoami --json` shows bob's grants, and `caesium logout` revokes
// the key on the server and removes the stored credential.
func (s *IntegrationTestSuite) TestKeycloakCLILoginRoundTrip() {
	lane := s.requireKeycloakLane()
	cli := s.keycloakCLILogin(lane, "bob@example.com", true)
	who, stderr, err := cli.whoami(s)
	s.Require().NoError(err, stderr)
	s.Equal("user", who.Kind)
	s.Equal("bob@example.com", who.Subject)
	s.Empty(who.ClusterRole)
	s.Equal(keycloakMarketingOperator, who.Grants)
	s.Equal([]string{"marketing"}, who.Namespaces)

	credentials := filepath.Join(cli.xdg, "caesium", "credentials.yaml")
	raw, err := os.ReadFile(credentials)
	s.Require().NoError(err)
	keyID := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "key_id: "); ok {
			keyID = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	s.Require().NotEmpty(keyID, "the stored credential names its key id")
	s.Require().Contains(s.activeUserKeyIDs(who.StableID), keyID)

	_, stderr, err = s.runCLIWithEnv(cli.env, "logout", "--server", s.caesiumURL)
	s.Require().NoError(err, "caesium logout: %s", stderr)
	s.NotContains(s.activeUserKeyIDs(who.StableID), keyID, "logout revoked the key on the server")
	s.requireCredentialRemoved(cli)
}

// requireCredentialRemoved asserts the CLI login's credential file no longer
// holds its key (or is gone).
func (s *IntegrationTestSuite) requireCredentialRemoved(cli *keycloakCLI) {
	s.T().Helper()
	credentials := filepath.Join(cli.xdg, "caesium", "credentials.yaml")
	raw, err := os.ReadFile(credentials)
	if err == nil {
		s.NotContains(string(raw), cli.key, "logout removed the stored credential")
		s.NotContains(string(raw), "key: csk_", "no credential is left behind")
		return
	}
	s.True(os.IsNotExist(err), "read %s: %v", credentials, err)
}

// requireCLIRefused asserts `caesium whoami --json` with the stored key fails
// with the server's 401 (not a 403, a 429, a 5xx or a transport error), and
// prints neither the key nor anything on stdout.
func (s *IntegrationTestSuite) requireCLIRefused(cli *keycloakCLI, what string) {
	s.T().Helper()
	stdout, stderr, err := s.runCLIWithEnv(cli.env, "whoami", "--json", "--server", s.caesiumURL)
	s.Require().Error(err, "%s is refused", what)
	s.Contains(stderr, "whoami failed (401)", "%s is refused with 401", what)
	s.Empty(strings.TrimSpace(stdout), "a refused whoami --json prints nothing on stdout")
	s.NotContains(stderr, cli.key, "the refused key is never printed")
}

// activeUserKeyIDs lists the user's active user-bound keys through the
// cluster admin's user detail.
func (s *IntegrationTestSuite) activeUserKeyIDs(userID string) []string {
	s.T().Helper()
	status, body := s.requestWithKey(http.MethodGet, "/v1/auth/users/"+userID, s.authAPIKey, nil)
	s.Require().Equal(http.StatusOK, status, body)
	var detail cliUserDetail
	s.Require().NoError(json.Unmarshal([]byte(body), &detail), body)
	ids := make([]string, 0, len(detail.UserKeys))
	for _, k := range detail.UserKeys {
		ids = append(ids, k.ID)
	}
	return ids
}

// TestKeycloakIdPRevocationRevokesCredentials (design §11.1): once Keycloak
// ends the user's sessions, its refresh token is refused with invalid_grant,
// a definitive answer: the next due refresh revokes the user's session and
// user-bound key together, audited with reason idp_refresh_denied.
func (s *IntegrationTestSuite) TestKeycloakIdPRevocationRevokesCredentials() {
	lane := s.requireKeycloakLane()
	kc := s.keycloakAdmin(lane)
	user := kc.createUser("idp-revoke", "marketing-eng")
	cli := s.keycloakCLILogin(lane, user.Username, false)
	refreshed := time.Now()
	who, stderr, err := cli.whoami(s)
	s.Require().NoError(err, stderr)
	userID := who.StableID

	kc.logoutUser(user)
	keycloakPastInterval(lane, refreshed)
	status, _, body := cli.browser.whoami(s)
	s.Require().Equal(http.StatusOK, status, "the request that finds the groups due still passes (the refresh runs after it): %s", body)
	keycloakBudgetFailures(2)
	s.keycloakAwait(lane, "the refused refresh revokes the session and the key", func() bool {
		d := s.ldapUserDetail(userID)
		return d.SessionsTotal == 0 && d.UserKeysTotal == 0
	})
	res := s.cliHTTP(http.MethodGet, "/auth/whoami", "", cli.browser.sessionCookie(s), "", "")
	s.Equal(http.StatusUnauthorized, res.status, res.body)
	s.requireCLIRefused(cli, "the revoked user-bound key")

	audits := s.awaitUserAuditRows("auth.sessions_revoked", uuid.MustParse(userID), 1)
	s.Require().Len(audits, 1)
	s.Equal("system:group-refresh", audits[0].Actor)
	s.requireAuditMeta(audits[0], map[string]any{
		"reason":       "idp_refresh_denied",
		"count":        float64(1),
		"keys_revoked": float64(1),
		"provider":     "oidc",
	})
}

// TestKeycloakNamespaceAllowDenyMatrix is the namespace allow/deny matrix
// (A7's harness, design §12.2) for SSO principals: a real Keycloak session
// per role in each of default, marketing, finance and "*" (realm users in the
// lane policy's kc-matrix-<role>-<namespace> groups), a runner in marketing
// and finance, and the key the SSO admin mints for marketing. Each session
// presents only its cookie (and its CSRF token on writes), so every route of
// internal/auth/rbac.go is decided for it per request from the user's
// groups. The lane mounts no connectors, database console or incidents
// (agent remediation is off), so the harness asserts those routes unmounted.
func (s *IntegrationTestSuite) TestKeycloakNamespaceAllowDenyMatrix() {
	lane := s.requireKeycloakLane()
	kc := s.keycloakAdmin(lane)
	caps := matrixLaneCaps{SSO: true, UserAdmin: true, Contracts: true, DataAssertions: true, LogConsole: true}
	s.runNamespaceMatrix(caps, func(fx *namespaceMatrixFixture) []matrixPrincipal {
		var principals []matrixPrincipal
		for _, role := range []models.Role{rV, rR, rO, rA} {
			for _, ns := range []string{nsDefault, nsMarketing, nsFinance, models.ClusterNamespace} {
				principals = append(principals, s.keycloakMatrixSession(lane, kc, string(role), ns))
			}
		}
		principals = append(principals, s.keycloakMatrixSession(lane, kc, string(rR), nsMarketing, nsFinance))

		admin := s.keycloakOIDCLogin(lane, "admin@example.com")
		s.requireSSOSession(admin)
		status, body := admin.send(s, http.MethodPost, "/v1/auth/keys", `{"description":"mx sso admin mint `+fx.stamp+`","role":"operator","namespaces":["marketing"]}`)
		s.Require().Equal(http.StatusCreated, status, "the SSO admin mints the matrix key: %s", body)
		key := s.parseCreatedKey(string(body))
		fx.f.keys = append(fx.f.keys, key.Meta.ID)
		principals = append(principals, bearerPrincipal("SSO-admin-minted operator@marketing", matrixKindAPIKey, key.Plaintext, key.Meta.KeyPrefix,
			map[string]models.Role{nsMarketing: rO}))
		return principals
	})
}
