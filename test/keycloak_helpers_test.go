//go:build integration

package test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
)

// The Keycloak lane (plan item G1, design §12.2): justfile
// integration-test-keycloak starts Keycloak with test/testdata/keycloak's
// realm and a Caesium server with OIDC against it, and runs the TestKeycloak*
// scenarios with these variables on the runner. G3/G4 reuse these helpers.
const (
	keycloakLaneEnv  = "CAESIUM_KEYCLOAK_LANE"
	keycloakURLEnv   = "CAESIUM_KEYCLOAK_URL"
	keycloakRealmEnv = "CAESIUM_KEYCLOAK_REALM"
	// G3: set by integration-test-keycloak from the same just variables
	// that configure Keycloak and the lane server.
	keycloakAdminUserEnv       = "CAESIUM_KEYCLOAK_ADMIN_USER"
	keycloakAdminPasswordEnv   = "CAESIUM_KEYCLOAK_ADMIN_PASSWORD"
	keycloakRefreshIntervalEnv = "CAESIUM_KEYCLOAK_GROUP_REFRESH_INTERVAL"
	keycloakMaxStalenessEnv    = "CAESIUM_KEYCLOAK_GROUP_MAX_STALENESS"

	keycloakRealmFile  = "test/testdata/keycloak/caesium-realm.json"
	keycloakPolicyFile = "test/testdata/keycloak/policy.yaml"

	// The jobs integration-up-keycloak applies, one per namespace.
	keycloakMarketingJob = "kc-lane-marketing"
	keycloakFinanceJob   = "kc-lane-finance"

	keycloakOIDCLoginPath    = "/auth/sso/oidc/login"
	keycloakOIDCCallbackPath = "/auth/sso/oidc/callback"
	keycloakSessionCookie    = "caesium_session"
)

// keycloakLane is the verified lane: where Keycloak answers and the realm
// users' passwords, read from the realm fixture so it stays the one source.
type keycloakLane struct {
	origin    *url.URL
	issuer    string
	realm     string
	passwords map[string]string
	// G3: the master-realm admin the admin REST helpers sign in as, and the
	// lane server's group refresh interval and maximum staleness.
	adminUser       string
	adminPassword   string
	refreshInterval time.Duration
	maxStaleness    time.Duration
}

// requireKeycloakLane gates a scenario on the Keycloak lane. Off the lane it
// skips. On the lane it never skips: a runner without the lane env, a server
// that does not advertise OIDC, does not serve the lane's policy file byte for
// byte, or lacks the two applied jobs is a broken lane and fails.
func (s *IntegrationTestSuite) requireKeycloakLane() *keycloakLane {
	s.T().Helper()
	if !envBool(keycloakLaneEnv) {
		s.T().Skipf("%s requires the Keycloak lane (just integration-test-keycloak)", s.T().Name())
	}

	base := strings.TrimRight(strings.TrimSpace(os.Getenv(keycloakURLEnv)), "/")
	realm := strings.TrimSpace(os.Getenv(keycloakRealmEnv))
	s.Require().NotEmpty(base, "%s=true but %s is unset: the Keycloak lane is misconfigured, not skippable", keycloakLaneEnv, keycloakURLEnv)
	s.Require().NotEmpty(realm, "%s=true but %s is unset", keycloakLaneEnv, keycloakRealmEnv)
	s.Require().NotEmpty(s.authAPIKey, "%s=true but the runner has no admin key (CAESIUM_API_KEY)", keycloakLaneEnv)
	origin, err := url.Parse(base)
	s.Require().NoError(err, "parse %s", keycloakURLEnv)

	fixture := s.keycloakRealmFixture()
	s.Require().Equal(realm, fixture.Realm, "%s names a realm the fixture does not define", keycloakRealmEnv)
	lane := &keycloakLane{origin: origin, issuer: base + "/realms/" + realm, realm: realm, passwords: map[string]string{}}
	lane.adminUser = strings.TrimSpace(os.Getenv(keycloakAdminUserEnv))
	lane.adminPassword = os.Getenv(keycloakAdminPasswordEnv)
	s.Require().NotEmpty(lane.adminUser, "%s=true but %s is unset", keycloakLaneEnv, keycloakAdminUserEnv)
	s.Require().NotEmpty(lane.adminPassword, "%s=true but %s is unset", keycloakLaneEnv, keycloakAdminPasswordEnv)
	lane.refreshInterval, err = time.ParseDuration(os.Getenv(keycloakRefreshIntervalEnv))
	s.Require().NoError(err, "%s=true needs %s (the server's CAESIUM_AUTH_GROUP_REFRESH_INTERVAL)", keycloakLaneEnv, keycloakRefreshIntervalEnv)
	lane.maxStaleness, err = time.ParseDuration(os.Getenv(keycloakMaxStalenessEnv))
	s.Require().NoError(err, "%s=true needs %s (the server's CAESIUM_AUTH_GROUP_MAX_STALENESS)", keycloakLaneEnv, keycloakMaxStalenessEnv)
	s.Require().Greater(lane.maxStaleness, lane.refreshInterval)
	for _, user := range fixture.Users {
		for _, cred := range user.Credentials {
			if cred.Type == "password" {
				lane.passwords[user.Username] = cred.Value
			}
		}
	}

	type authMethod struct {
		Type     string `json:"type"`
		LoginURL string `json:"loginUrl"`
	}
	var status struct {
		Enabled bool         `json:"enabled"`
		Methods []authMethod `json:"methods"`
	}
	s.getJSON("/auth/status", &status)
	s.Require().True(status.Enabled, "the Keycloak lane's server reports authentication disabled")
	s.Require().True(slices.ContainsFunc(status.Methods, func(m authMethod) bool {
		return m.Type == "oidc" && m.LoginURL == keycloakOIDCLoginPath
	}), "the Keycloak lane's server does not advertise OIDC: %+v", status.Methods)

	served, err := os.ReadFile(filepath.Join(s.projectRoot, keycloakPolicyFile))
	s.Require().NoError(err, "read %s", keycloakPolicyFile)
	sum := sha256.Sum256(served)
	var policyResp struct {
		Revision string `json:"revision"`
	}
	s.getJSON("/v1/auth/policy", &policyResp)
	s.Require().Equal(hex.EncodeToString(sum[:]), policyResp.Revision,
		"the Keycloak lane's server must serve %s byte for byte (justfile integration-up-keycloak)", keycloakPolicyFile)

	s.requireJobByAlias(keycloakMarketingJob)
	s.requireJobByAlias(keycloakFinanceJob)
	return lane
}

type keycloakRealm struct {
	Realm string `json:"realm"`
	Users []struct {
		Username    string `json:"username"`
		Credentials []struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"credentials"`
	} `json:"users"`
}

func (s *IntegrationTestSuite) keycloakRealmFixture() keycloakRealm {
	s.T().Helper()
	raw, err := os.ReadFile(filepath.Join(s.projectRoot, keycloakRealmFile))
	s.Require().NoError(err, "read %s", keycloakRealmFile)
	var realm keycloakRealm
	s.Require().NoError(json.Unmarshal(raw, &realm), "parse %s", keycloakRealmFile)
	return realm
}

// keycloakLogin is one browser-equivalent SSO login. client carries the
// cookie jar (Keycloak's cookies and, after a successful callback, Caesium's
// session cookie) and never sends an Authorization header.
type keycloakLogin struct {
	client           *http.Client
	CallbackPath     string
	CallbackStatus   int
	CallbackLocation string
	SessionCookie    bool
	// Hops records "STATUS METHOD host/path" per request, never query strings,
	// codes, state or cookie values.
	Hops []string
	// csrf caches the session's CSRF token (csrfToken).
	csrf string
}

var (
	keycloakFormTag    = regexp.MustCompile(`(?is)<form\b[^>]*>`)
	keycloakFormAction = regexp.MustCompile(`(?is)\baction\s*=\s*"([^"]*)"`)
)

// keycloakLoginFormAction returns the HTML-unescaped action of Keycloak's
// username/password form (id kc-form-login), whatever the attribute order or
// login theme; "" when the page has no such form.
func keycloakLoginFormAction(page []byte) string {
	for _, tag := range keycloakFormTag.FindAll(page, -1) {
		if !bytes.Contains(tag, []byte("kc-form-login")) {
			continue
		}
		if m := keycloakFormAction.FindSubmatch(tag); m != nil {
			return html.UnescapeString(string(m[1]))
		}
	}
	return ""
}

// keycloakLogin drives the real authorization-code flow from startURL (a
// Caesium URL that redirects into Keycloak, e.g. /auth/sso/oidc/login or, for
// G4, the CLI authorize URL): it follows every redirect itself, submits the
// user's realm password to Keycloak's HTML login form, and stops at the first
// Caesium response after Keycloak — the callback — whose answer it records.
// The client keeps the jar for the requests that follow.
func (s *IntegrationTestSuite) keycloakLogin(lane *keycloakLane, startURL, username string) *keycloakLogin {
	s.T().Helper()
	password, ok := lane.passwords[username]
	s.Require().True(ok, "%s has no password in %s", username, keycloakRealmFile)
	jar, err := cookiejar.New(nil)
	s.Require().NoError(err)
	login := &keycloakLogin{client: &http.Client{
		Jar:     jar,
		Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
	caesium, err := url.Parse(s.caesiumURL)
	s.Require().NoError(err)

	target, err := url.Parse(startURL)
	s.Require().NoError(err)
	method := http.MethodGet
	var form url.Values
	visitedKeycloak, submitted := false, false
	for hop := 0; hop < 16; hop++ {
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req, err := http.NewRequestWithContext(s.T().Context(), method, target.String(), body)
		s.Require().NoError(err)
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, err := login.client.Do(req)
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			// The URL may carry the authorization code and state; report
			// only where the request went.
			urlErr.URL = target.Host + target.Path
		}
		s.Require().NoError(err, "%s %s%s; hops: %v", method, target.Host, target.Path, login.Hops)
		page, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		s.Require().NoError(err)
		login.Hops = append(login.Hops, fmt.Sprintf("%d %s %s%s", resp.StatusCode, method, target.Host, target.Path))

		onKeycloak := sameURLOrigin(target, lane.origin)
		if onKeycloak {
			visitedKeycloak = true
		} else if visitedKeycloak && sameURLOrigin(target, caesium) {
			login.CallbackPath = target.Path
			login.CallbackStatus = resp.StatusCode
			login.CallbackLocation = resp.Header.Get("Location")
			for _, cookie := range resp.Cookies() {
				if cookie.Name == keycloakSessionCookie && cookie.Value != "" && cookie.MaxAge >= 0 {
					login.SessionCookie = true
				}
			}
			return login
		}

		switch {
		case resp.StatusCode >= 300 && resp.StatusCode < 400:
			location := resp.Header.Get("Location")
			s.Require().NotEmpty(location, "redirect without Location; hops: %v", login.Hops)
			next, err := url.Parse(location)
			s.Require().NoError(err, "parse redirect; hops: %v", login.Hops)
			target, method, form = target.ResolveReference(next), http.MethodGet, nil
		case onKeycloak && resp.StatusCode == http.StatusOK && !submitted:
			action := keycloakLoginFormAction(page)
			s.Require().NotEmpty(action, "Keycloak answered without its login form; hops: %v", login.Hops)
			next, err := url.Parse(action)
			s.Require().NoError(err)
			next = target.ResolveReference(next)
			s.Require().True(sameURLOrigin(next, lane.origin), "the login form posts off Keycloak (%s%s); refusing to send credentials", next.Host, next.Path)
			target, method, submitted = next, http.MethodPost, true
			form = url.Values{"username": {username}, "password": {password}, "credentialId": {""}}
		default:
			hint := ""
			if bytes.Contains(page, []byte("Invalid username or password")) {
				hint = " (Keycloak: invalid username or password)"
			}
			s.Require().FailNowf("SSO login did not reach Caesium's callback", "%s: unexpected %d%s; hops: %v", username, resp.StatusCode, hint, login.Hops)
		}
	}
	s.Require().FailNowf("SSO login did not reach Caesium's callback", "%s: too many hops: %v", username, login.Hops)
	return nil
}

// keycloakOIDCLogin starts the Console's OIDC login (returnTo "/").
func (s *IntegrationTestSuite) keycloakOIDCLogin(lane *keycloakLane, username string) *keycloakLogin {
	s.T().Helper()
	return s.keycloakLogin(lane, s.caesiumURL+keycloakOIDCLoginPath+"?returnTo=%2F", username)
}

func sameURLOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

// get issues a GET to Caesium with the login's cookies and no API key.
func (l *keycloakLogin) get(s *IntegrationTestSuite, path string) (int, []byte) {
	s.T().Helper()
	req, err := http.NewRequestWithContext(s.T().Context(), http.MethodGet, s.caesiumURL+path, nil)
	s.Require().NoError(err)
	resp, err := l.client.Do(req)
	s.Require().NoError(err, "GET %s", path)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	return resp.StatusCode, body
}

type keycloakWhoami struct {
	Kind        string            `json:"kind"`
	Subject     string            `json:"subject"`
	StableID    string            `json:"stable_id"`
	Email       string            `json:"email"`
	Role        string            `json:"role"`
	ClusterRole string            `json:"cluster_role"`
	Grants      map[string]string `json:"grants"`
	Namespaces  []string          `json:"namespaces"`
}

// requireSSOSession asserts a successful callback (a redirect that sets the
// session cookie) and returns the session principal's whoami.
func (s *IntegrationTestSuite) requireSSOSession(login *keycloakLogin) keycloakWhoami {
	s.T().Helper()
	s.Require().Equal(keycloakOIDCCallbackPath, login.CallbackPath, "hops: %v", login.Hops)
	s.Require().Equal(http.StatusFound, login.CallbackStatus, "hops: %v", login.Hops)
	s.Require().True(login.SessionCookie, "the callback set no %s cookie; hops: %v", keycloakSessionCookie, login.Hops)
	status, body := login.get(s, "/auth/whoami")
	s.Require().Equal(http.StatusOK, status, "whoami with the SSO session: %s", body)
	var who keycloakWhoami
	s.Require().NoError(json.Unmarshal(body, &who), "whoami: %s", body)
	s.Require().Equal("user", who.Kind, "whoami: %s", body)
	return who
}

// requireNoBindingCallback asserts Caesium's callback answer to a login whose
// identity resolved no grants (design §6.3, reason no_binding), and that no
// session was created. The redirect flow sends the browser to the Console's
// login page with the reason (W5-ζ, F1-D3); the Location is same-origin and
// relative, so it resolves under CAESIUM_AUTH_PUBLIC_BASE_URL.
func (s *IntegrationTestSuite) requireNoBindingCallback(login *keycloakLogin) {
	s.T().Helper()
	s.Require().Equal(keycloakOIDCCallbackPath, login.CallbackPath, "hops: %v", login.Hops)
	s.Require().Equal(http.StatusFound, login.CallbackStatus, "no_binding callback; hops: %v", login.Hops)
	s.Require().Equal("/?login_error=no_binding", login.CallbackLocation, "no_binding callback; hops: %v", login.Hops)
	s.Require().False(login.SessionCookie, "a denied login set a session cookie")
	status, body := login.get(s, "/auth/whoami")
	s.Require().Equal(http.StatusUnauthorized, status, "whoami after a denied login: %s", body)
}

// requireLoginDeniedAudit asserts that `caesium auth audit` (stdout parsed on
// its own) holds a denied auth.login_denied row for actor since the given time
// with the given reason, from the OIDC provider.
func (s *IntegrationTestSuite) requireLoginDeniedAudit(actor, reason string, since time.Time) {
	s.T().Helper()
	type row struct {
		Actor    string         `json:"actor"`
		Action   string         `json:"action"`
		Outcome  string         `json:"outcome"`
		Metadata map[string]any `json:"metadata"`
	}
	deadline := time.Now().Add(15 * time.Second)
	var rows []row
	for {
		stdout, stderr, err := s.runCLISeparate("auth", "audit", "--server", s.caesiumURL,
			"--since", since.UTC().Format(time.RFC3339), "--action", "auth.login_denied", "--actor", actor, "--limit", "100")
		s.Require().NoError(err, "caesium auth audit: %s", strings.TrimSpace(stderr))
		rows = nil
		s.Require().NoError(json.Unmarshal([]byte(stdout), &rows), "caesium auth audit stdout must be exactly one JSON array:\n%s", stdout)
		for _, r := range rows {
			if r.Actor == actor && r.Action == "auth.login_denied" && r.Outcome == "denied" &&
				r.Metadata["reason"] == reason && r.Metadata["provider"] == "oidc" {
				return
			}
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	s.Require().FailNowf("missing login-denied audit row", "actor %s reason %s: %+v", actor, reason, rows)
}

// keycloakJobAliases lists the jobs the login's session may see.
func (s *IntegrationTestSuite) keycloakJobAliases(login *keycloakLogin) []string {
	s.T().Helper()
	status, body := login.get(s, "/v1/jobs")
	s.Require().Equal(http.StatusOK, status, "job list with the SSO session: %s", body)
	var jobs []jobSummary
	s.Require().NoError(json.Unmarshal(body, &jobs), "job list: %s", body)
	aliases := make([]string, 0, len(jobs))
	for _, job := range jobs {
		aliases = append(aliases, job.Alias)
	}
	return aliases
}

// ---- G3/G4: SSO sessions as API clients, Keycloak's admin REST API, the CLI
// login through Keycloak, and the lane's authentication-failure budget.

// sessionCookie is the login's Caesium session cookie.
func (l *keycloakLogin) sessionCookie(s *IntegrationTestSuite) *http.Cookie {
	s.T().Helper()
	caesium, err := url.Parse(s.caesiumURL)
	s.Require().NoError(err)
	for _, cookie := range l.client.Jar.Cookies(caesium) {
		if cookie.Name == keycloakSessionCookie && cookie.Value != "" {
			return cookie
		}
	}
	s.Require().FailNow("the login holds no Caesium session cookie", "hops: %v", l.Hops)
	return nil
}

// csrfToken is the session's CSRF token, as the Console reads it from whoami.
func (l *keycloakLogin) csrfToken(s *IntegrationTestSuite) string {
	s.T().Helper()
	if l.csrf != "" {
		return l.csrf
	}
	status, body := l.get(s, "/auth/whoami")
	s.Require().Equal(http.StatusOK, status, "whoami for the CSRF token: %s", body)
	var who struct {
		CSRFToken string `json:"csrf_token"`
	}
	s.Require().NoError(json.Unmarshal(body, &who), "whoami: %s", body)
	s.Require().NotEmpty(who.CSRFToken, "a cookie session's whoami carries its CSRF token")
	l.csrf = who.CSRFToken
	return l.csrf
}

// send issues a request to Caesium with the login's cookies, a JSON body when
// one is given, and, on an unsafe method, the session's CSRF token (so a
// write is never refused, and counted as a failure, for a missing token).
func (l *keycloakLogin) send(s *IntegrationTestSuite, method, path, body string) (int, []byte) {
	s.T().Helper()
	csrf := ""
	if method != http.MethodGet && method != http.MethodHead {
		csrf = l.csrfToken(s)
	}
	return l.sendCSRF(s, method, path, body, csrf)
}

func (l *keycloakLogin) sendCSRF(s *IntegrationTestSuite, method, path, body, csrf string) (int, []byte) {
	s.T().Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(s.T().Context(), method, s.caesiumURL+path, reader)
	s.Require().NoError(err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	resp, err := l.client.Do(req)
	s.Require().NoError(err, "%s %s", method, path)
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	return resp.StatusCode, out
}

// whoami is the session's whoami: the status and, on 200, the principal.
func (l *keycloakLogin) whoami(s *IntegrationTestSuite) (int, keycloakWhoami, []byte) {
	s.T().Helper()
	status, body := l.get(s, "/auth/whoami")
	var who keycloakWhoami
	if status == http.StatusOK {
		s.Require().NoError(json.Unmarshal(body, &who), "whoami: %s", body)
	}
	return status, who, body
}

// keycloakAdmin drives the lane realm through Keycloak's admin REST API as
// the master-realm bootstrap admin. Its token is fetched per operation
// (master access tokens live 60 s) and never reaches a message.
type keycloakAdmin struct {
	s      *IntegrationTestSuite
	lane   *keycloakLane
	client *http.Client
}

func (s *IntegrationTestSuite) keycloakAdmin(lane *keycloakLane) *keycloakAdmin {
	return &keycloakAdmin{s: s, lane: lane, client: &http.Client{
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (a *keycloakAdmin) token() string {
	a.s.T().Helper()
	form := url.Values{"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {a.lane.adminUser}, "password": {a.lane.adminPassword}}
	endpoint := a.lane.origin.String() + "/realms/master/protocol/openid-connect/token"
	req, err := http.NewRequestWithContext(a.s.T().Context(), http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	a.s.Require().NoError(err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.client.Do(req)
	a.s.Require().NoError(err, "Keycloak admin token request")
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	a.s.Require().Equal(http.StatusOK, resp.StatusCode, "Keycloak admin token request")
	a.s.Require().NoError(json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok))
	a.s.Require().NotEmpty(tok.AccessToken, "Keycloak admin token response")
	return tok.AccessToken
}

// call sends one admin request under /admin/realms/<realm>; body, when not
// nil, is sent as JSON. It returns the status, body and Location header.
func (a *keycloakAdmin) call(method, path string, body any) (int, []byte, string) {
	a.s.T().Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		a.s.Require().NoError(err)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(a.s.T().Context(), method, a.lane.origin.String()+"/admin/realms/"+a.lane.realm+path, reader)
	a.s.Require().NoError(err)
	req.Header.Set("Authorization", "Bearer "+a.token())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	a.s.Require().NoError(err, "Keycloak admin %s %s", method, path)
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	a.s.Require().NoError(err)
	return resp.StatusCode, out, resp.Header.Get("Location")
}

// groupID returns the id of the realm's top-level group name.
func (a *keycloakAdmin) groupID(name string) string {
	a.s.T().Helper()
	status, body, _ := a.call(http.MethodGet, "/groups?briefRepresentation=true&exact=true&search="+url.QueryEscape(name), nil)
	a.s.Require().Equal(http.StatusOK, status, "Keycloak group lookup %s: %s", name, body)
	var groups []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	a.s.Require().NoError(json.Unmarshal(body, &groups))
	for _, g := range groups {
		if g.Name == name {
			return g.ID
		}
	}
	a.s.Require().FailNowf("Keycloak group missing", "%s is not a realm group (%s)", name, keycloakRealmFile)
	return ""
}

// keycloakUser is a realm user created for one scenario.
type keycloakUser struct {
	ID       string
	Username string
}

// createUser adds a verified, enabled realm user in groups with a random
// password (registered on the lane for keycloakLogin) and deletes it when the
// scenario ends. Every scenario that changes a user's groups, sessions or
// state uses its own, so the fixture users stay as G1 and the browser
// journeys expect them.
func (a *keycloakAdmin) createUser(label string, groups ...string) keycloakUser {
	a.s.T().Helper()
	username := fmt.Sprintf("g3-%s-%d@example.com", label, time.Now().UnixNano())
	password := cliRandomToken()
	status, body, location := a.call(http.MethodPost, "/users", map[string]any{
		"username": username, "email": username, "emailVerified": true, "enabled": true,
		"firstName": "G3", "lastName": label, "requiredActions": []string{},
		"credentials": []map[string]any{{"type": "password", "value": password, "temporary": false}},
	})
	a.s.Require().Equal(http.StatusCreated, status, "Keycloak create user %s: %s", username, body)
	id := location[strings.LastIndex(location, "/")+1:]
	a.s.Require().NotEmpty(id, "Keycloak create user answered no Location")
	a.s.T().Cleanup(func() {
		if status, _, _ := a.call(http.MethodDelete, "/users/"+id, nil); status != http.StatusNoContent && status != http.StatusNotFound {
			a.s.T().Errorf("delete Keycloak user %s: status %d", username, status)
		}
	})
	a.lane.passwords[username] = password
	user := keycloakUser{ID: id, Username: username}
	for _, g := range groups {
		a.join(user, g)
	}
	return user
}

func (a *keycloakAdmin) join(user keycloakUser, group string) {
	a.s.T().Helper()
	status, body, _ := a.call(http.MethodPut, "/users/"+user.ID+"/groups/"+a.groupID(group), nil)
	a.s.Require().Equal(http.StatusNoContent, status, "Keycloak join %s %s: %s", user.Username, group, body)
}

func (a *keycloakAdmin) leave(user keycloakUser, group string) {
	a.s.T().Helper()
	status, body, _ := a.call(http.MethodDelete, "/users/"+user.ID+"/groups/"+a.groupID(group), nil)
	a.s.Require().Equal(http.StatusNoContent, status, "Keycloak leave %s %s: %s", user.Username, group, body)
}

// move takes user out of from and into to.
func (a *keycloakAdmin) move(user keycloakUser, from, to string) {
	a.s.T().Helper()
	a.join(user, to)
	a.leave(user, from)
}

// logoutUser ends every Keycloak session of user: its refresh tokens are
// then refused with invalid_grant.
func (a *keycloakAdmin) logoutUser(user keycloakUser) {
	a.s.T().Helper()
	status, body, _ := a.call(http.MethodPost, "/users/"+user.ID+"/logout", nil)
	a.s.Require().Equal(http.StatusNoContent, status, "Keycloak logout %s: %s", user.Username, body)
}

// setClientEnabled enables or disables the caesium client. A disabled
// client's token requests are refused with an OAuth error other than
// invalid_grant, which Caesium treats as an IdP failure, not a revocation.
// Disabling registers a cleanup that enables it again. The client
// representation carries its secret, so no body reaches a message.
func (a *keycloakAdmin) setClientEnabled(enabled bool) {
	a.s.T().Helper()
	status, body, _ := a.call(http.MethodGet, "/clients?clientId=caesium", nil)
	a.s.Require().Equal(http.StatusOK, status, "Keycloak client lookup")
	var clients []map[string]any
	a.s.Require().NoError(json.Unmarshal(body, &clients), "Keycloak client lookup")
	a.s.Require().Len(clients, 1, "the realm has exactly the caesium client")
	client := clients[0]
	id, _ := client["id"].(string)
	a.s.Require().NotEmpty(id)
	client["enabled"] = enabled
	status, _, _ = a.call(http.MethodPut, "/clients/"+id, client)
	a.s.Require().Equal(http.StatusNoContent, status, "Keycloak update the caesium client (enabled=%v)", enabled)
	if !enabled {
		a.s.T().Cleanup(func() {
			client["enabled"] = true
			if status, _, _ := a.call(http.MethodPut, "/clients/"+id, client); status != http.StatusNoContent {
				a.s.T().Errorf("re-enable the caesium client: status %d", status)
			}
		})
	}
}

// keycloakCLI is one `caesium login --no-browser` completed through Keycloak:
// its own XDG_CONFIG_HOME and environment (no CAESIUM_API_KEY), the stored
// user-bound key, and the browser login that authorised it.
type keycloakCLI struct {
	xdg     string
	env     []string
	key     string
	browser *keycloakLogin
}

var (
	keycloakSignInLink  = regexp.MustCompile(`href="(/auth/sso/oidc/login\?returnTo=[^"]*)"`)
	keycloakConfirmMain = regexp.MustCompile(`<main id="cli" data-state="([^"]*)" data-csrf="([^"]*)">`)
)

// keycloakCLILogin is the browser-less CLI login (design §13.1 step 5) end to
// end: `caesium login --no-browser` prints the start URL, the browser opens
// it (start → finish → OIDC sign-in through Keycloak's form → back on the
// finish page with a session), clicks Authorise (the CSRF-carrying POST),
// reads the code, and `caesium login --code` stores the key. viaSignInPage
// first loads the finish page signed out and follows its OIDC link, as a
// browser without a Caesium session does.
func (s *IntegrationTestSuite) keycloakCLILogin(lane *keycloakLane, username string, viaSignInPage bool) *keycloakCLI {
	s.T().Helper()
	xdg, env := s.cliLoginEnv()
	stdout, stderr, err := s.runCLIWithEnv(env, "login", "--no-browser", "--server", s.caesiumURL)
	s.Require().NoError(err, "caesium login --no-browser: %s", stderr)
	start := strings.TrimSuffix(stdout, "\n")
	s.Require().NotContains(start, "\n", "login --no-browser prints exactly the start URL on stdout: %q", stdout)
	s.Require().True(strings.HasPrefix(start, s.caesiumURL+"/auth/cli/start?"), "start URL: %q", start)
	state := s.followStart(start)
	finish := "/auth/cli/finish?state=" + state

	signIn := keycloakOIDCLoginPath + "?returnTo=" + url.QueryEscape(finish)
	if viaSignInPage {
		page := s.cliHTTP(http.MethodGet, finish, "", nil, "", "")
		s.Require().Equal(http.StatusUnauthorized, page.status, "a signed-out finish page")
		m := keycloakSignInLink.FindStringSubmatch(page.body)
		s.Require().NotNil(m, "the signed-out finish page offers the OIDC sign-in: %s", page.body)
		s.Require().Equal(signIn, html.UnescapeString(m[1]))
	}
	browser := s.keycloakLogin(lane, s.caesiumURL+signIn, username)
	s.Require().Equal(keycloakOIDCCallbackPath, browser.CallbackPath, "hops: %v", browser.Hops)
	s.Require().Equal(http.StatusFound, browser.CallbackStatus, "hops: %v", browser.Hops)
	s.Require().True(browser.SessionCookie, "hops: %v", browser.Hops)
	s.Require().Equal(finish, browser.CallbackLocation, "the callback returns to the CLI confirmation page")

	status, page := browser.get(s, finish)
	s.Require().Equal(http.StatusOK, status, "the confirmation page: %s", page)
	s.Require().Contains(string(page), html.EscapeString(username))
	s.Require().NotContains(string(page), "csk_user_", "the confirmation page carries no key")
	m := keycloakConfirmMain.FindSubmatch(page)
	s.Require().NotNil(m, "the confirmation page carries its state and CSRF token")
	s.Require().Equal(state, html.UnescapeString(string(m[1])))
	status, body := browser.sendCSRF(s, http.MethodPost, "/auth/cli/authorize", `{"state":"`+state+`"}`, html.UnescapeString(string(m[2])))
	s.Require().Equal(http.StatusOK, status, "Authorise: %s", body)
	var authorized map[string]string
	s.Require().NoError(json.Unmarshal(body, &authorized), "Authorise: %s", body)
	s.Require().Empty(authorized["redirect_url"], "a --no-browser login shows the code instead of redirecting")
	code := authorized["code"]
	s.Require().NotEmpty(code)

	stdout, stderr, err = s.runCLIWithEnv(env, "login", "--code", code, "--server", s.caesiumURL)
	s.Require().NoError(err, "caesium login --code: %s", stderr)
	key := s.storedCLIKey(xdg)
	s.Require().True(strings.HasPrefix(key, "csk_user_"), "the stored credential is a user-bound key")
	s.Require().NotContains(stdout+stderr, key, "the key is never printed")
	return &keycloakCLI{xdg: xdg, env: env, key: key, browser: browser}
}

// whoami runs `caesium whoami --json` with the stored credential: on success
// the parsed stdout, else the error and stderr.
func (c *keycloakCLI) whoami(s *IntegrationTestSuite) (keycloakWhoami, string, error) {
	s.T().Helper()
	stdout, stderr, err := s.runCLIWithEnv(c.env, "whoami", "--json", "--server", s.caesiumURL)
	var who keycloakWhoami
	if err == nil {
		s.Require().NoError(json.Unmarshal([]byte(stdout), &who), "whoami --json stdout must be exactly the JSON body: %q", stdout)
	}
	return who, stderr, err
}

// keycloakAwait polls observe until it reports done, within the lane's
// refresh interval plus slack.
func (s *IntegrationTestSuite) keycloakAwait(lane *keycloakLane, what string, observe func() bool) {
	s.T().Helper()
	s.Require().Eventually(observe, lane.refreshInterval+20*time.Second, 500*time.Millisecond, what)
}

// keycloakPastInterval sleeps until groups confirmed at since are due for a
// refresh, staying well inside the maximum staleness.
func keycloakPastInterval(lane *keycloakLane, since time.Time) {
	time.Sleep(time.Until(since.Add(lane.refreshInterval + time.Second)))
}

// The server limits authentication failures per source IP (authFailureWindow,
// 10 a window by default) and every scenario on the lane shares the runner's
// IP. keycloakBudgetFailures reserves the n failures a scenario is about to
// provoke (a revoked, disabled or stale credential; an absent credential does
// not count), first waiting until the last window has room for them under
// keycloakFailureBudget.
const keycloakFailureBudget = 8

var keycloakFailures struct {
	sync.Mutex
	at []time.Time
}

func keycloakBudgetFailures(n int) {
	keycloakFailures.Lock()
	defer keycloakFailures.Unlock()
	for {
		cutoff := time.Now().Add(-authFailureWindow - 2*time.Second)
		kept := keycloakFailures.at[:0]
		for _, at := range keycloakFailures.at {
			if at.After(cutoff) {
				kept = append(kept, at)
			}
		}
		keycloakFailures.at = kept
		if len(kept) == 0 || len(kept)+n <= keycloakFailureBudget {
			break
		}
		time.Sleep(time.Until(kept[0].Add(authFailureWindow + 2*time.Second)))
	}
	for i := 0; i < n; i++ {
		keycloakFailures.at = append(keycloakFailures.at, time.Now())
	}
}

// keycloakPrincipal is an SSO session that holds exactly Grants, for the
// scenarios that need a principal of a given role in given namespaces (the
// SSO side of the namespace allow/deny matrix).
type keycloakPrincipal struct {
	Name   string
	Grants map[string]string
	Email  string
	Login  *keycloakLogin
}

// keycloakMatrixPrincipal signs in a new realm user in the lane policy's
// kc-matrix-<role>-<namespace> groups, so it holds role in each of
// namespaces ("*" for every namespace), and checks whoami says exactly that.
func (s *IntegrationTestSuite) keycloakMatrixPrincipal(lane *keycloakLane, kc *keycloakAdmin, role string, namespaces ...string) keycloakPrincipal {
	s.T().Helper()
	s.Require().NotEmpty(namespaces)
	grants := map[string]string{}
	groups := make([]string, 0, len(namespaces))
	for _, ns := range namespaces {
		group := "kc-matrix-" + role + "-" + ns
		if ns == "*" {
			group = "kc-matrix-" + role + "-all"
		}
		groups = append(groups, group)
		grants[ns] = role
	}
	name := role + "@" + strings.Join(namespaces, "+")
	user := kc.createUser("matrix-"+role, groups...)
	login := s.keycloakOIDCLogin(lane, user.Username)
	who := s.requireSSOSession(login)
	s.Require().Equal(grants, who.Grants, "SSO principal %s", name)
	return keycloakPrincipal{Name: name, Grants: grants, Email: user.Username, Login: login}
}

// keycloakMatrixSession is a keycloakMatrixPrincipal as a namespace-matrix
// principal: its requests carry only the session cookie, plus the session's
// CSRF token on writes, and never follow a redirect. Its audit rows carry the
// user's email.
func (s *IntegrationTestSuite) keycloakMatrixSession(lane *keycloakLane, kc *keycloakAdmin, role string, namespaces ...string) matrixPrincipal {
	s.T().Helper()
	p := s.keycloakMatrixPrincipal(lane, kc, role, namespaces...)
	csrf := p.Login.csrfToken(s)
	grants := make(map[string]models.Role, len(p.Grants))
	for ns, r := range p.Grants {
		grants[ns] = models.Role(r)
	}
	client := &http.Client{
		Jar:           p.Login.client.Jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return matrixPrincipal{
		Name:       "SSO " + p.Name,
		Kind:       matrixKindSession,
		Grants:     grants,
		AuditActor: p.Email,
		Do: func(req *http.Request) (*http.Response, error) {
			switch req.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
			default:
				req.Header.Set("X-CSRF-Token", csrf)
			}
			return client.Do(req)
		},
	}
}
