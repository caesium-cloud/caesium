//go:build integration

package test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	gldap "github.com/go-ldap/ldap/v3"
	"github.com/google/uuid"
)

// LDAP sign-in and SSO group refresh (identity-and-access E1b + E2, design
// §11.1) driven through the real surfaces: POST /auth/sso/ldap/login against
// the lanes' OpenLDAP fixture (scripts/ldap-fixture.sh, verified over LDAPS
// with the fixture CA exactly as production verifies a directory), whoami
// through the session cookie and through a user-bound key minted by the live
// `caesium login` routes, and the admin user-detail and audit routes. The
// directory is changed through an admin bind from the runner; the catalog is
// written only to make a user's groups due or stale (a precondition: the
// lanes keep the default 15m interval and 24h bound), never to assert.
//
// Both auth lanes run these: the agent lane, whose policy file binds the
// fixture groups per namespace, and the coverage journey's file-less api-key
// server, which maps them with CAESIUM_AUTH_ROLE_MAPPING.

const (
	ldapURLEnv           = "CAESIUM_IT_LDAP_URL"
	ldapCAFileEnv        = "CAESIUM_IT_LDAP_CA_FILE"
	ldapAdminDN          = "cn=admin,dc=example,dc=org"
	ldapAdminPassword    = "iaa-ldap-admin-secret"
	ldapPeopleBase       = "ou=people,dc=example,dc=org"
	ldapGroupBase        = "ou=groups,dc=example,dc=org"
	ldapGroupMarketing   = "iaa-ldap-marketing"
	ldapGroupFinance     = "iaa-ldap-finance"
	ldapGroupUnbound     = "iaa-ldap-unbound"
	ldapRefreshWait      = 20 * time.Second
	ldapDueBackdate      = 20 * time.Minute
	ldapPastBoundBackoff = 25 * time.Hour
)

// ldapLaneGrants is what each fixture group grants on this lane.
func ldapLaneGrants(group string) map[string]string {
	if authLaneWithoutPolicy() {
		// CAESIUM_AUTH_ROLE_MAPPING on the coverage journey server.
		switch group {
		case ldapGroupMarketing:
			return map[string]string{"*": "operator"}
		case ldapGroupFinance:
			return map[string]string{"*": "viewer"}
		}
		return map[string]string{}
	}
	switch group {
	case ldapGroupMarketing:
		return map[string]string{"marketing": "operator"}
	case ldapGroupFinance:
		return map[string]string{"finance": "operator"}
	}
	return map[string]string{}
}

// requireLDAPLane gates a scenario on an auth lane with the OpenLDAP fixture.
// Every auth lane carries it, so on an auth lane a missing fixture fails.
func (s *IntegrationTestSuite) requireLDAPLane() {
	s.T().Helper()
	s.requireAuthLane()
	s.Require().NotEmpty(strings.TrimSpace(os.Getenv(ldapURLEnv)),
		"the auth lane runner must carry %s (justfile integration-test-agent, scripts/coverage-journeys.sh)", ldapURLEnv)
	s.Require().NotEmpty(strings.TrimSpace(os.Getenv(ldapCAFileEnv)), "the auth lane runner must carry %s", ldapCAFileEnv)
}

type ldapDirectory struct {
	s    *IntegrationTestSuite
	conn *gldap.Conn
}

// ldapAdmin binds to the fixture directory as its admin over LDAPS, trusting
// only the fixture CA. The directory may still be starting: retry for 30s.
func (s *IntegrationTestSuite) ldapAdmin() *ldapDirectory {
	s.T().Helper()
	caPEM, err := os.ReadFile(os.Getenv(ldapCAFileEnv))
	s.Require().NoError(err)
	pool := x509.NewCertPool()
	s.Require().True(pool.AppendCertsFromPEM(caPEM), "fixture CA")
	tlsCfg := &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12}
	var conn *gldap.Conn
	deadline := time.Now().Add(30 * time.Second)
	for {
		// Every attempt is bounded: the dial (TCP and the LDAPS handshake)
		// by the dialer, the bind and every later operation by the
		// connection's request timeout, so the 30s deadline is reached.
		attempt := min(ldapOpTimeout, max(time.Until(deadline), time.Second))
		conn, err = gldap.DialURL(os.Getenv(ldapURLEnv), gldap.DialWithTLSConfig(tlsCfg),
			gldap.DialWithDialer(&net.Dialer{Timeout: attempt}))
		if err == nil {
			conn.SetTimeout(ldapOpTimeout)
			if err = conn.Bind(ldapAdminDN, ldapAdminPassword); err == nil {
				break
			}
			_ = conn.Close()
		}
		s.Require().True(time.Now().Before(deadline), "bind the LDAP fixture: %v", err)
		time.Sleep(500 * time.Millisecond)
	}
	s.T().Cleanup(func() { _ = conn.Close() })
	return &ldapDirectory{s: s, conn: conn}
}

// ldapOpTimeout bounds each dial and each directory operation of the
// fixture's admin connection.
const ldapOpTimeout = 5 * time.Second

func ldapUserDN(uid string) string { return "uid=" + uid + "," + ldapPeopleBase }
func ldapGroupDN(cn string) string { return "cn=" + cn + "," + ldapGroupBase }

// addUser creates a person with a unique uid in groups and removes it (and
// its memberships, and the Caesium rows its logins create) when the scenario
// ends.
func (d *ldapDirectory) addUser(label string, groups ...string) (uid, password string) {
	s := d.s
	s.T().Helper()
	uid = fmt.Sprintf("%s-%d", label, time.Now().UnixNano())
	password = "pw-" + uuid.NewString()
	add := gldap.NewAddRequest(ldapUserDN(uid), nil)
	add.Attribute("objectClass", []string{"inetOrgPerson"})
	add.Attribute("uid", []string{uid})
	add.Attribute("cn", []string{uid})
	add.Attribute("sn", []string{"Fixture"})
	add.Attribute("mail", []string{uid + "@ldap.example.org"})
	add.Attribute("displayName", []string{"LDAP " + uid})
	add.Attribute("userPassword", []string{password})
	s.Require().NoError(d.conn.Add(add))
	for _, group := range groups {
		d.join(uid, group)
	}
	s.T().Cleanup(func() {
		for _, group := range []string{ldapGroupMarketing, ldapGroupFinance, ldapGroupUnbound} {
			d.leaveQuietly(uid, group)
		}
		_ = d.conn.Del(gldap.NewDelRequest(ldapUserDN(uid), nil))
		conn := s.openIntegrationCatalogGorm()
		subject := ldapUserDN(uid)
		for _, stmt := range []string{
			`DELETE FROM cli_login_transactions WHERE user_id IN (SELECT id FROM users WHERE issuer = 'ldap' AND subject = ?)`,
			`DELETE FROM api_keys WHERE user_id IN (SELECT id FROM users WHERE issuer = 'ldap' AND subject = ?)`,
			`DELETE FROM sessions WHERE user_id IN (SELECT id FROM users WHERE issuer = 'ldap' AND subject = ?)`,
			`DELETE FROM users WHERE issuer = 'ldap' AND subject = ?`,
		} {
			s.NoError(conn.Exec(stmt, subject).Error)
		}
	})
	return uid, password
}

func (d *ldapDirectory) join(uid, group string) {
	d.s.T().Helper()
	mod := gldap.NewModifyRequest(ldapGroupDN(group), nil)
	mod.Add("member", []string{ldapUserDN(uid)})
	d.s.Require().NoError(d.conn.Modify(mod))
}

func (d *ldapDirectory) leave(uid, group string) {
	d.s.T().Helper()
	mod := gldap.NewModifyRequest(ldapGroupDN(group), nil)
	mod.Delete("member", []string{ldapUserDN(uid)})
	d.s.Require().NoError(d.conn.Modify(mod))
}

func (d *ldapDirectory) leaveQuietly(uid, group string) {
	mod := gldap.NewModifyRequest(ldapGroupDN(group), nil)
	mod.Delete("member", []string{ldapUserDN(uid)})
	_ = d.conn.Modify(mod)
}

// disable makes the lanes' login filter, (&(uid={username})(!(description=disabled))),
// stop admitting the entry: login and refresh both refuse it.
func (d *ldapDirectory) disable(uid string) {
	d.s.T().Helper()
	mod := gldap.NewModifyRequest(ldapUserDN(uid), nil)
	mod.Add("description", []string{"disabled"})
	d.s.Require().NoError(d.conn.Modify(mod))
}

// ldapLogin signs in through POST /auth/sso/ldap/login.
func (s *IntegrationTestSuite) ldapLogin(uid, password string) (int, string, *http.Cookie) {
	s.T().Helper()
	body, err := json.Marshal(map[string]string{"username": uid, "password": password})
	s.Require().NoError(err)
	res := s.cliHTTP(http.MethodPost, "/auth/sso/ldap/login", "", nil, "", string(body))
	var cookie *http.Cookie
	name := env.Variables().AuthSessionCookieName
	for _, c := range (&http.Response{Header: res.header}).Cookies() {
		if c.Name == name && c.Value != "" {
			cookie = &http.Cookie{Name: name, Value: c.Value}
		}
	}
	return res.status, res.body, cookie
}

type ldapWhoami struct {
	Kind      string            `json:"kind"`
	Subject   string            `json:"subject"`
	StableID  string            `json:"stable_id"`
	Grants    map[string]string `json:"grants"`
	CSRFToken string            `json:"csrf_token"`
}

func (s *IntegrationTestSuite) ldapCookieWhoami(cookie *http.Cookie) ldapWhoami {
	s.T().Helper()
	res := s.cliHTTP(http.MethodGet, "/auth/whoami", "", cookie, "", "")
	s.Require().Equal(http.StatusOK, res.status, res.body)
	var who ldapWhoami
	s.Require().NoError(json.Unmarshal([]byte(res.body), &who), res.body)
	return who
}

func (s *IntegrationTestSuite) ldapKeyWhoami(key string) ldapWhoami {
	s.T().Helper()
	status, body := s.requestWithKey(http.MethodGet, "/auth/whoami", key, nil)
	s.Require().Equal(http.StatusOK, status, body)
	var who ldapWhoami
	s.Require().NoError(json.Unmarshal([]byte(body), &who), body)
	return who
}

type ldapUserDetail struct {
	Groups        []string `json:"groups"`
	SessionsTotal int64    `json:"sessions_total"`
	UserKeysTotal int64    `json:"user_keys_total"`
}

func (s *IntegrationTestSuite) ldapUserDetail(userID string) ldapUserDetail {
	s.T().Helper()
	status, body := s.requestWithKey(http.MethodGet, "/v1/auth/users/"+userID, s.authAPIKey, nil)
	s.Require().Equal(http.StatusOK, status, body)
	var detail ldapUserDetail
	s.Require().NoError(json.Unmarshal([]byte(body), &detail), body)
	return detail
}

// makeGroupsAged sets when the user's groups were last confirmed, so the
// next request finds them due (past the 15m interval) or stale (past the 24h
// bound). A precondition only.
func (s *IntegrationTestSuite) makeGroupsAged(userID string, age time.Duration, clearGrant bool) {
	s.T().Helper()
	conn := s.openIntegrationCatalogGorm()
	updates := map[string]any{"groups_refreshed_at": time.Now().UTC().Add(-age)}
	if clearGrant {
		updates["refresh_grant"] = nil
	}
	res := conn.Model(&models.User{}).Where("id = ?", userID).Updates(updates)
	s.Require().NoError(res.Error)
	s.Require().EqualValues(1, res.RowsAffected)
}

// ldapMintUserKey mints a user-bound key for the signed-in LDAP browser
// through the live `caesium login` routes (start, confirm, exchange).
func (s *IntegrationTestSuite) ldapMintUserKey(cookie *http.Cookie, who ldapWhoami) string {
	s.T().Helper()
	browser := &cliLoginBrowser{user: &models.User{Email: who.Subject}, cookie: cookie, csrf: who.CSRFToken}
	verifier := cliRandomToken()
	code := s.startConfirmed(browser, verifier, "ldap-refresh")
	res := s.exchangeCLI(code, verifier)
	s.Require().Equal(http.StatusOK, res.status, res.body)
	var out struct {
		Key string `json:"key"`
	}
	s.Require().NoError(json.Unmarshal([]byte(res.body), &out), res.body)
	s.Require().True(strings.HasPrefix(out.Key, "csk_user_"))
	return out.Key
}

// TestAuthLDAPLoginNoBindingBody: an LDAP user the directory admits but no
// binding grants anything is refused 403 with reason no_binding (F1's body,
// on the LDAP provider).
func (s *IntegrationTestSuite) TestAuthLDAPLoginNoBindingBody() {
	s.requireLDAPLane()
	dir := s.ldapAdmin()
	uid, password := dir.addUser("unbound", ldapGroupUnbound)

	status, body, cookie := s.ldapLogin(uid, password)
	s.Require().Equal(http.StatusForbidden, status, body)
	s.Nil(cookie, "a refused login sets no session")
	var refusal map[string]string
	s.Require().NoError(json.Unmarshal([]byte(body), &refusal), body)
	s.Equal(map[string]string{"message": "login denied", "reason": "no_binding"}, refusal)
}

// TestAuthLDAPGroupRefreshFollowsDirectory: a user's group change in the
// directory reaches its grants without a new sign-in, through the browser
// session and through a CLI-only user-bound key, and is audited.
func (s *IntegrationTestSuite) TestAuthLDAPGroupRefreshFollowsDirectory() {
	s.requireLDAPLane()
	dir := s.ldapAdmin()
	uid, password := dir.addUser("mover", ldapGroupMarketing)

	status, body, cookie := s.ldapLogin(uid, password)
	s.Require().Equal(http.StatusNoContent, status, body)
	s.Require().NotNil(cookie)
	who := s.ldapCookieWhoami(cookie)
	s.Require().Equal("user", who.Kind)
	s.Require().Equal(ldapLaneGrants(ldapGroupMarketing), who.Grants)
	s.Require().NotEmpty(who.StableID)
	userID := who.StableID
	s.Equal([]string{ldapGroupMarketing}, s.ldapUserDetail(userID).Groups)
	key := s.ldapMintUserKey(cookie, who)
	s.Equal(ldapLaneGrants(ldapGroupMarketing), s.ldapKeyWhoami(key).Grants)

	// Browser: the directory moves the user; once its groups are due, the
	// request that finds them so still answers with the stored groups and
	// queues a refresh, after which the new grants apply.
	dir.leave(uid, ldapGroupMarketing)
	dir.join(uid, ldapGroupFinance)
	s.Equal(ldapLaneGrants(ldapGroupMarketing), s.ldapCookieWhoami(cookie).Grants, "fresh groups are not re-read")
	s.makeGroupsAged(userID, ldapDueBackdate, false)
	s.Equal(ldapLaneGrants(ldapGroupMarketing), s.ldapCookieWhoami(cookie).Grants, "a due user proceeds with its stored groups")
	s.Require().Eventually(func() bool {
		return fmt.Sprint(s.ldapCookieWhoami(cookie).Grants) == fmt.Sprint(ldapLaneGrants(ldapGroupFinance))
	}, ldapRefreshWait, 250*time.Millisecond, "the refreshed groups reach the session")
	s.Equal([]string{ldapGroupFinance}, s.ldapUserDetail(userID).Groups)

	audits := s.awaitUserAuditRows("auth.groups_refreshed", uuid.MustParse(userID), 1)
	s.Require().Len(audits, 1)
	s.Equal("system:group-refresh", audits[0].Actor)
	s.requireAuditMeta(audits[0], map[string]any{
		"provider": "ldap",
		"added":    []any{ldapGroupFinance},
		"removed":  []any{ldapGroupMarketing},
		"trigger":  "async",
	})

	// CLI only: the key alone drives the next refresh.
	dir.leave(uid, ldapGroupFinance)
	dir.join(uid, ldapGroupMarketing)
	s.makeGroupsAged(userID, ldapDueBackdate, false)
	s.Require().Eventually(func() bool {
		return fmt.Sprint(s.ldapKeyWhoami(key).Grants) == fmt.Sprint(ldapLaneGrants(ldapGroupMarketing))
	}, ldapRefreshWait, 250*time.Millisecond, "the refreshed groups reach the user-bound key")
	s.Len(s.awaitUserAuditRows("auth.groups_refreshed", uuid.MustParse(userID), 2), 2)
	s.Equal(ldapLaneGrants(ldapGroupMarketing), s.ldapCookieWhoami(cookie).Grants)
}

// TestAuthLDAPDisabledEntryRevokesCredentials: once the directory's login
// filter no longer admits the user (here: description=disabled), the next
// refresh is a definitive refusal that revokes its sessions and user-bound
// keys together, audited with reason idp_refresh_denied. One 401 only.
func (s *IntegrationTestSuite) TestAuthLDAPDisabledEntryRevokesCredentials() {
	s.requireLDAPLane()
	dir := s.ldapAdmin()
	uid, password := dir.addUser("leaver", ldapGroupFinance)

	status, body, cookie := s.ldapLogin(uid, password)
	s.Require().Equal(http.StatusNoContent, status, body)
	who := s.ldapCookieWhoami(cookie)
	userID := who.StableID
	s.ldapMintUserKey(cookie, who)
	detail := s.ldapUserDetail(userID)
	s.Require().EqualValues(1, detail.SessionsTotal)
	s.Require().EqualValues(1, detail.UserKeysTotal)

	dir.disable(uid)
	s.makeGroupsAged(userID, ldapDueBackdate, false)
	s.Equal(ldapLaneGrants(ldapGroupFinance), s.ldapCookieWhoami(cookie).Grants, "the request that queues the refresh still passes")
	s.Require().Eventually(func() bool {
		d := s.ldapUserDetail(userID)
		return d.SessionsTotal == 0 && d.UserKeysTotal == 0
	}, ldapRefreshWait, 250*time.Millisecond, "the definitive refusal revokes the session and the key")

	res := s.cliHTTP(http.MethodGet, "/auth/whoami", "", cookie, "", "")
	s.Equal(http.StatusUnauthorized, res.status, res.body)

	audits := s.awaitUserAuditRows("auth.sessions_revoked", uuid.MustParse(userID), 1)
	s.Require().Len(audits, 1)
	s.Equal("system:group-refresh", audits[0].Actor)
	s.requireAuditMeta(audits[0], map[string]any{
		"reason":       "idp_refresh_denied",
		"count":        float64(1),
		"keys_revoked": float64(1),
		"provider":     "ldap",
	})
}

// TestAuthStaleGroupsRefusedUntilRelogin: past the 24h bound the next request
// refreshes synchronously (and is admitted when that works); a user whose
// groups cannot be refreshed (its grant is gone) is refused with an explained
// 401 stale_groups until it signs in again. One 401 only.
func (s *IntegrationTestSuite) TestAuthStaleGroupsRefusedUntilRelogin() {
	s.requireLDAPLane()
	dir := s.ldapAdmin()
	uid, password := dir.addUser("stale", ldapGroupMarketing)

	status, body, cookie := s.ldapLogin(uid, password)
	s.Require().Equal(http.StatusNoContent, status, body)
	userID := s.ldapCookieWhoami(cookie).StableID

	// Past the bound with a working grant: the first request waits for the
	// refresh and is admitted with the directory's current groups.
	dir.leave(uid, ldapGroupMarketing)
	dir.join(uid, ldapGroupFinance)
	s.makeGroupsAged(userID, ldapPastBoundBackoff, false)
	s.Equal(ldapLaneGrants(ldapGroupFinance), s.ldapCookieWhoami(cookie).Grants, "a synchronous refresh admits with the new groups")
	audits := s.awaitUserAuditRows("auth.groups_refreshed", uuid.MustParse(userID), 1)
	s.Require().Len(audits, 1)
	var meta map[string]any
	s.Require().NoError(json.Unmarshal(audits[0].Metadata, &meta))
	s.Equal("sync", meta["trigger"])

	// Past the bound with nothing to refresh from (as after a rotated hash
	// secret): refused, explained, and persisted for the user.
	s.makeGroupsAged(userID, ldapPastBoundBackoff, true)
	res := s.cliHTTP(http.MethodGet, "/auth/whoami", "", cookie, "", "")
	s.Require().Equal(http.StatusUnauthorized, res.status, res.body)
	var refusal map[string]string
	s.Require().NoError(json.Unmarshal([]byte(res.body), &refusal), res.body)
	s.Equal("stale_groups", refusal["reason"])
	s.NotEmpty(refusal["message"])

	// A new sign-in confirms the groups and lifts the refusal.
	status, body, fresh := s.ldapLogin(uid, password)
	s.Require().Equal(http.StatusNoContent, status, body)
	s.Equal(ldapLaneGrants(ldapGroupFinance), s.ldapCookieWhoami(fresh).Grants)
	s.Equal(ldapLaneGrants(ldapGroupFinance), s.ldapCookieWhoami(cookie).Grants, "the earlier session is valid again")
}

// awaitUserAuditRows polls the audit log until the user has at least want
// rows of action: the refresher commits an outcome before it writes the
// audit row, so seeing the new groups does not prove the row exists yet.
func (s *IntegrationTestSuite) awaitUserAuditRows(action string, userID uuid.UUID, want int) []adminAuditRow {
	s.T().Helper()
	var rows []adminAuditRow
	s.Require().Eventually(func() bool {
		rows = s.userAuditRows(action, userID)
		return len(rows) >= want
	}, ldapRefreshWait, 250*time.Millisecond, "%s audit rows for the user", action)
	return rows
}
