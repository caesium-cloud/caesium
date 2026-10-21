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
	"time"
)

// The Keycloak lane (plan item G1, design §12.2): justfile
// integration-test-keycloak starts Keycloak with test/testdata/keycloak's
// realm and a Caesium server with OIDC against it, and runs the TestKeycloak*
// scenarios with these variables on the runner. G3/G4 reuse these helpers.
const (
	keycloakLaneEnv  = "CAESIUM_KEYCLOAK_LANE"
	keycloakURLEnv   = "CAESIUM_KEYCLOAK_URL"
	keycloakRealmEnv = "CAESIUM_KEYCLOAK_REALM"

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
	passwords map[string]string
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
	lane := &keycloakLane{origin: origin, issuer: base + "/realms/" + realm, passwords: map[string]string{}}
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
// session was created.
//
// Master answers 403 "login denied". W5-ζ (F1-D3) changes the redirect flows
// to 302 <CAESIUM_AUTH_PUBLIC_BASE_URL>/?login_error=no_binding; when this
// lane rebases onto it, replace the one status assertion below with
//
//	s.Require().Equal(http.StatusFound, login.CallbackStatus, ...)
//	s.Require().Equal(s.caesiumURL+"/?login_error=no_binding", login.CallbackLocation, ...)
func (s *IntegrationTestSuite) requireNoBindingCallback(login *keycloakLogin) {
	s.T().Helper()
	s.Require().Equal(keycloakOIDCCallbackPath, login.CallbackPath, "hops: %v", login.Hops)
	s.Require().Equal(http.StatusForbidden, login.CallbackStatus, "no_binding callback; hops: %v", login.Hops)
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
