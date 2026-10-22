//go:build integration

package test

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

// `caesium login`, `caesium whoami` and `caesium logout` (identity-and-access
// E5, design §13) driven through the real binary and the live routes. No
// browser runs in CI: the browser's part — following the start redirect,
// loading the confirmation page with the session cookie and clicking
// Authorise (a CSRF-carrying POST) — is played by an HTTP client holding a
// real session cookie seeded through the catalog (a precondition only; every
// assertion reads a live surface). Every CLI run gets its own
// XDG_CONFIG_HOME and an empty CAESIUM_API_KEY, so it uses only the
// credential it stored and never touches the runner's real config.

// cliLoginBrowser is the signed-in browser: a session cookie and its CSRF
// token, for a seeded user.
type cliLoginBrowser struct {
	user   *models.User
	cookie *http.Cookie
	csrf   string
}

func (s *IntegrationTestSuite) newCLILoginBrowser(label string, groups ...string) (*userAdminFixture, *cliLoginBrowser) {
	s.T().Helper()
	f := s.newUserAdminFixture(label)
	// Runs before the fixture's own cleanup (LIFO), so the users can go.
	s.T().Cleanup(func() {
		s.NoError(f.conn.Exec(`DELETE FROM cli_login_transactions WHERE user_id IN (SELECT id FROM users WHERE issuer = ?)`, f.issuer).Error)
		s.NoError(f.conn.Exec(`DELETE FROM api_keys WHERE user_id IN (SELECT id FROM users WHERE issuer = ?)`, f.issuer).Error)
	})
	user := s.seedAdminUser(f, label, time.Now().UTC(), groups...)
	cookie, sess := s.mintSession(f, user.ID)
	return f, &cliLoginBrowser{user: user, cookie: cookie, csrf: sess.CSRFToken}
}

type cliHTTPResult struct {
	status int
	body   string
	header http.Header
}

// cliHTTP sends one request without following redirects.
func (s *IntegrationTestSuite) cliHTTP(method, target, bearer string, cookie *http.Cookie, csrf, body string) cliHTTPResult {
	s.T().Helper()
	if strings.HasPrefix(target, "/") {
		target = s.caesiumURL + target
	}
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(s.T().Context(), method, target, reader)
	s.Require().NoError(err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	return cliHTTPResult{status: resp.StatusCode, body: string(out), header: resp.Header}
}

func (s *IntegrationTestSuite) cliLoginEnv() (string, []string) {
	xdg := s.T().TempDir()
	return xdg, []string{"CAESIUM_API_KEY=", "XDG_CONFIG_HOME=" + xdg}
}

func cliRandomToken() string {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func cliS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// followStart plays the browser opening the CLI's start URL: a 302 to the
// confirmation page for the same state.
func (s *IntegrationTestSuite) followStart(start string) string {
	s.T().Helper()
	u, err := url.Parse(start)
	s.Require().NoError(err)
	state := u.Query().Get("state")
	s.Require().Len(state, 43)
	res := s.cliHTTP(http.MethodGet, start, "", nil, "", "")
	s.Require().Equal(http.StatusFound, res.status, res.body)
	s.Require().Equal("/auth/cli/finish?state="+state, res.header.Get("Location"))
	return state
}

// confirm loads the confirmation page and clicks Authorise.
func (s *IntegrationTestSuite) confirm(b *cliLoginBrowser, state string) map[string]string {
	s.T().Helper()
	page := s.cliHTTP(http.MethodGet, "/auth/cli/finish?state="+url.QueryEscape(state), "", b.cookie, "", "")
	s.Require().Equal(http.StatusOK, page.status, page.body)
	s.Require().Contains(page.body, b.user.Email)
	s.Require().NotContains(page.body, "csk_user_", "the confirmation page carries no key")
	res := s.cliHTTP(http.MethodPost, "/auth/cli/authorize", "", b.cookie, b.csrf, `{"state":"`+state+`"}`)
	s.Require().Equal(http.StatusOK, res.status, res.body)
	var out map[string]string
	s.Require().NoError(json.Unmarshal([]byte(res.body), &out))
	return out
}

type cliUserDetail struct {
	UserKeysTotal int64 `json:"user_keys_total"`
	UserKeys      []struct {
		ID string `json:"id"`
	} `json:"user_keys"`
}

// activeUserKeys reads the user's active user-bound keys through the cluster
// admin's user-detail route (never a 401 of the user's own key).
func (s *IntegrationTestSuite) activeUserKeys(userID uuid.UUID) cliUserDetail {
	s.T().Helper()
	status, body := s.requestWithKey(http.MethodGet, "/v1/auth/users/"+userID.String(), s.authAPIKey, nil)
	s.Require().Equal(http.StatusOK, status, body)
	var detail cliUserDetail
	s.Require().NoError(json.Unmarshal([]byte(body), &detail), body)
	return detail
}

type cliWhoami struct {
	Kind     string            `json:"kind"`
	Subject  string            `json:"subject"`
	StableID string            `json:"stable_id"`
	Grants   map[string]string `json:"grants"`
}

func (s *IntegrationTestSuite) cookieWhoami(b *cliLoginBrowser) cliWhoami {
	s.T().Helper()
	res := s.cliHTTP(http.MethodGet, "/auth/whoami", "", b.cookie, "", "")
	s.Require().Equal(http.StatusOK, res.status, res.body)
	var who cliWhoami
	s.Require().NoError(json.Unmarshal([]byte(res.body), &who))
	return who
}

func (s *IntegrationTestSuite) cliWhoamiJSON(env []string, args ...string) cliWhoami {
	s.T().Helper()
	stdout, stderr, err := s.runCLIWithEnv(env, append([]string{"whoami", "--json", "--server", s.caesiumURL}, args...)...)
	s.Require().NoError(err, stderr)
	var who cliWhoami
	s.Require().NoError(json.Unmarshal([]byte(stdout), &who), "whoami --json stdout must be exactly the JSON body: %q", stdout)
	return who
}

func (s *IntegrationTestSuite) storedCLIKey(xdg string) string {
	s.T().Helper()
	raw, err := os.ReadFile(filepath.Join(xdg, "caesium", "credentials.yaml"))
	s.Require().NoError(err)
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "key: "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// TestAuthCLILoginManualCodeFlow is the --no-browser flow end to end:
// `caesium login --no-browser` → start → confirmation page → Authorise (one
// confirmation only) → `caesium login --code` → the stored credential drives
// `caesium whoami --json` and `caesium cache list` in separate processes with
// CAESIUM_API_KEY cleared, flag and env still win over it, and
// `caesium logout` revokes the key and forgets it.
func (s *IntegrationTestSuite) TestAuthCLILoginManualCodeFlow() {
	s.requireAuthLane()
	_, b := s.newCLILoginBrowser("e5-manual", "iaa-reload-viewers")
	xdg, env := s.cliLoginEnv()

	stdout, stderr, err := s.runCLIWithEnv(env, "login", "--no-browser", "--server", s.caesiumURL)
	s.Require().NoError(err, stderr)
	start := strings.TrimSpace(stdout)
	s.Require().True(strings.HasPrefix(start, s.caesiumURL+"/auth/cli/start?"), "stdout is the start URL: %q", stdout)
	s.Require().Equal("manual", mustQuery(s, start).Get("mode"))
	state := s.followStart(start)

	// An unknown state renders an error and mints nothing.
	unknown := s.cliHTTP(http.MethodGet, "/auth/cli/finish?state="+cliRandomToken(), "", b.cookie, "", "")
	s.Require().Equal(http.StatusNotFound, unknown.status, unknown.body)
	s.Require().NotContains(unknown.body, "csk_user_")

	page := s.cliHTTP(http.MethodGet, "/auth/cli/finish?state="+state, "", b.cookie, "", "")
	s.Require().Equal(http.StatusOK, page.status)
	s.Equal("DENY", page.header.Get("X-Frame-Options"))
	s.Contains(page.header.Get("Content-Security-Policy"), "frame-ancestors 'none'")

	shown := s.confirm(b, state)
	code := shown["code"]
	s.Require().Len(code, 43)
	again := s.cliHTTP(http.MethodPost, "/auth/cli/authorize", "", b.cookie, b.csrf, `{"state":"`+state+`"}`)
	s.Require().Equal(http.StatusConflict, again.status, "a second confirmation is refused: %s", again.body)
	used := s.cliHTTP(http.MethodGet, "/auth/cli/finish?state="+state, "", b.cookie, "", "")
	s.Require().Equal(http.StatusConflict, used.status)
	s.Require().NotContains(used.body, "csk_user_")
	s.Require().Zero(s.activeUserKeys(b.user.ID).UserKeysTotal, "neither the page nor the confirmation mints a key")

	// Exactly the command the confirmation page shows: no --server, so the
	// CLI redeems its one pending login.
	_, stderr, err = s.runCLIWithEnv(env, "login", "--code", code)
	s.Require().NoError(err, stderr)
	s.Contains(stderr, "Logged in to")

	dir := filepath.Join(xdg, "caesium")
	info, err := os.Stat(dir)
	s.Require().NoError(err)
	s.Equal(os.FileMode(0o700), info.Mode().Perm())
	info, err = os.Stat(filepath.Join(dir, "credentials.yaml"))
	s.Require().NoError(err)
	s.Equal(os.FileMode(0o600), info.Mode().Perm())
	userKey := s.storedCLIKey(xdg)
	s.Require().True(strings.HasPrefix(userKey, "csk_user_"))
	// Only the key's display prefix (as `caesium auth key list` shows it) is
	// printed, never the key.
	s.NotContains(stderr, userKey, "the key is never printed")
	s.Require().EqualValues(1, s.activeUserKeys(b.user.ID).UserKeysTotal)

	// A separate process with CAESIUM_API_KEY cleared acts as the user, with
	// exactly the grants the user's browser session has.
	oracle := s.cookieWhoami(b)
	who := s.cliWhoamiJSON(env)
	s.Equal("user", who.Kind)
	s.Equal(b.user.Email, who.Subject)
	s.Equal(b.user.ID.String(), who.StableID)
	s.Equal(oracle.Grants, who.Grants)

	// Precedence: CAESIUM_API_KEY beats the stored login; --api-key beats both.
	envAdmin := append(append([]string(nil), env...), "CAESIUM_API_KEY="+s.authAPIKey)
	s.Equal("api_key", s.cliWhoamiJSON(envAdmin).Kind, "the env var beats the stored login")
	envUser := append(append([]string(nil), env...), "CAESIUM_API_KEY="+userKey)
	s.Equal("api_key", s.cliWhoamiJSON(envUser, "--api-key", s.authAPIKey).Kind, "the flag beats the env var")

	// A command that used to send no credentials at all now resolves the
	// stored login (cache list): it authenticates (never 401), while the same
	// command with no stored login is refused as unauthenticated.
	alias := fmt.Sprintf("e5-cache-probe-%d", time.Now().UnixNano())
	s.applyScopedJobDefinition(s.authAPIKey, alias, false)
	jobID := s.cliJobIDByAlias(alias)
	_, bareErr, err := s.runCLIWithEnv([]string{"CAESIUM_API_KEY=", "XDG_CONFIG_HOME=" + s.T().TempDir()},
		"cache", "list", "--job-id", jobID, "--server", s.caesiumURL)
	s.Require().Error(err)
	s.Contains(bareErr, "(401)", "without a credential the server refuses it as unauthenticated")
	listOut, listErr, err := s.runCLIWithEnv(env, "cache", "list", "--job-id", jobID, "--server", s.caesiumURL)
	if _, ok := oracle.Grants["*"]; ok {
		// The user is a cluster viewer (the policy fixture binds its group):
		// the stored login must actually list the job's cache.
		s.Require().NoError(err, listErr)
		var listed cacheListResponse
		s.Require().NoError(json.Unmarshal([]byte(listOut), &listed), "cache list stdout is the JSON listing: %q", listOut)
		s.Empty(listed.Entries, "a job that never ran has no cache entries")
	} else {
		// No policy file binds the group (the coverage journey's lane): the
		// stored login authenticates and RBAC refuses it, never a 401.
		s.Require().Error(err)
		s.Contains(listErr, "(403)", "the stored login authenticated cache list: %s", listErr)
	}

	_, stderr, err = s.runCLIWithEnv(env, "logout", "--server", s.caesiumURL)
	s.Require().NoError(err, stderr)
	s.Contains(stderr, "Logged out of")
	s.Empty(s.storedCLIKey(xdg), "the stored login is gone")
	s.Zero(s.activeUserKeys(b.user.ID).UserKeysTotal, "caesium logout revoked the key")
	s.Equal(http.StatusOK, s.cliHTTP(http.MethodGet, "/auth/whoami", "", b.cookie, "", "").status,
		"the browser session is untouched")
}

// cliJobIDByAlias finds a job's id through the admin key.
func (s *IntegrationTestSuite) cliJobIDByAlias(alias string) string {
	s.T().Helper()
	status, body := s.requestWithKey(http.MethodGet, "/v1/jobs?order_by=created_at%20desc", s.authAPIKey, nil)
	s.Require().Equal(http.StatusOK, status, body)
	var jobs []jobSummary
	s.Require().NoError(json.Unmarshal([]byte(body), &jobs), body)
	for _, job := range jobs {
		if job.Alias == alias {
			return job.ID
		}
	}
	s.FailNow("job not found", alias)
	return ""
}

func mustQuery(s *IntegrationTestSuite, raw string) url.Values {
	s.T().Helper()
	u, err := url.Parse(raw)
	s.Require().NoError(err)
	return u.Query()
}

// TestAuthCLILoginLoopbackFlow runs the default flow with --no-open: the CLI
// listens on 127.0.0.1, refuses a callback with the wrong state, and on the
// confirmation's redirect exchanges the code and stores the key.
func (s *IntegrationTestSuite) TestAuthCLILoginLoopbackFlow() {
	s.requireAuthLane()
	_, b := s.newCLILoginBrowser("e5-loopback", "iaa-reload-viewers")
	xdg, env := s.cliLoginEnv()

	cmd := exec.CommandContext(s.T().Context(), s.cliPath, "login", "--no-open", "--timeout", "90s", "--server", s.caesiumURL)
	cmd.Dir = s.projectRoot
	cmd.Env = append(os.Environ(), env...)
	stdoutPipe, err := cmd.StdoutPipe()
	s.Require().NoError(err)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	s.Require().NoError(cmd.Start())
	done := make(chan error, 1)
	lines := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		if scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
		_, _ = io.Copy(io.Discard, stdoutPipe)
		done <- cmd.Wait()
	}()
	var start string
	select {
	case start = <-lines:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		s.FailNow("caesium login --no-open printed no URL", stderr.String())
	}
	s.Require().NotEmpty(mustQuery(s, start).Get("port"))
	state := s.followStart(start)

	redirect := s.confirm(b, state)["redirect_url"]
	callback, err := url.Parse(redirect)
	s.Require().NoError(err)
	s.Require().Equal("http", callback.Scheme)
	s.Require().True(strings.HasPrefix(callback.Host, "127.0.0.1:"), redirect)
	s.Require().Equal(state, callback.Query().Get("state"))

	forged := *callback
	q := forged.Query()
	q.Set("state", cliRandomToken())
	forged.RawQuery = q.Encode()
	bad := s.cliHTTP(http.MethodGet, forged.String(), "", nil, "", "")
	s.Require().Equal(http.StatusBadRequest, bad.status, "the CLI refuses a callback with the wrong state")

	ok := s.cliHTTP(http.MethodGet, redirect, "", nil, "", "")
	s.Require().Equal(http.StatusOK, ok.status)
	select {
	case err = <-done:
		s.Require().NoError(err, stderr.String())
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		s.FailNow("caesium login did not finish after the callback", stderr.String())
	}
	stored := s.storedCLIKey(xdg)
	s.Require().True(strings.HasPrefix(stored, "csk_user_"))
	s.NotContains(stderr.String(), stored, "the key is never printed")
	who := s.cliWhoamiJSON(env)
	s.Equal("user", who.Kind)
	s.Equal(b.user.Email, who.Subject)
}

// startConfirmed starts a transaction for verifier over HTTP (as the CLI
// would) with a labelled hostname hint and confirms it, returning the code.
func (s *IntegrationTestSuite) startConfirmed(b *cliLoginBrowser, verifier, hint string) string {
	s.T().Helper()
	q := url.Values{"state": {cliRandomToken()}, "challenge": {cliS256(verifier)}, "mode": {"manual"}, "hostname": {hint}}
	state := s.followStart(s.caesiumURL + "/auth/cli/start?" + q.Encode())
	return s.confirm(b, state)["code"]
}

func (s *IntegrationTestSuite) exchangeCLI(code, verifier string) cliHTTPResult {
	s.T().Helper()
	body, err := json.Marshal(map[string]string{"code": code, "code_verifier": verifier})
	s.Require().NoError(err)
	return s.cliHTTP(http.MethodPost, "/auth/cli/exchange", "", nil, "", string(body))
}

// TestAuthCLILoginRefusalsAndSelfRevocation covers every refused exchange
// (unknown, reused, expired, wrong verifier, the right verifier after a wrong
// one burned the code, and a code confirmed before an admin's "log out
// everywhere"), a confirmation without the CSRF token, and self-revocation:
// POST /auth/logout with a user-bound bearer AND the user's cookie revokes
// exactly the bearer key, while an admin key's logout revokes nothing.
//
// Each refusal counts on the server's per-IP authentication-failure limiter,
// so, like TestAuthUserBoundKeyLifecycle, the scenario spends its eight
// failures in a failure window of its own and waits out that window before
// returning. The codes the burst redeems are confirmed after the wait (a code
// lives 60 seconds, so one confirmed before it would be refused as expired
// and prove nothing about the verifier).
func (s *IntegrationTestSuite) TestAuthCLILoginRefusalsAndSelfRevocation() {
	started := time.Now()
	s.requireAuthLane()
	f, b := s.newCLILoginBrowser("e5-refusals", "iaa-reload-viewers")
	stamp := time.Now().UnixNano()

	verifierA := cliRandomToken()
	codeA := s.startConfirmed(b, verifierA, fmt.Sprintf("e5-it-a-%d", stamp))
	okA := s.exchangeCLI(codeA, verifierA)
	s.Require().Equal(http.StatusOK, okA.status, okA.body)
	var minted map[string]string
	s.Require().NoError(json.Unmarshal([]byte(okA.body), &minted))
	keyA := minted["key"]
	s.Require().True(strings.HasPrefix(keyA, "csk_user_"))
	s.Equal(b.user.Email, minted["subject"])
	s.Equal("no-store", okA.header.Get("Cache-Control"))
	s.Require().EqualValues(1, s.activeUserKeys(b.user.ID).UserKeysTotal)

	// Self-revocation: the bearer is the credential, the cookie alongside it
	// is not.
	logout := s.cliHTTP(http.MethodPost, "/auth/logout", keyA, b.cookie, "", "")
	s.Require().Equal(http.StatusNoContent, logout.status, logout.body)
	s.Empty(logout.header.Values("Set-Cookie"), "logout with a bearer leaves the cookie alone")
	s.Zero(s.activeUserKeys(b.user.ID).UserKeysTotal, "the user-bound key is revoked")
	s.Equal(http.StatusOK, s.cliHTTP(http.MethodGet, "/auth/whoami", "", b.cookie, "", "").status, "the cookie session survives")
	// An admin key's logout is an audited no-op: the key keeps working.
	s.Equal(http.StatusNoContent, s.cliHTTP(http.MethodPost, "/auth/logout", s.authAPIKey, nil, "", "").status)
	status, _ := s.requestWithKey(http.MethodGet, "/auth/whoami", s.authAPIKey, nil)
	s.Equal(http.StatusOK, status, "logout never revokes an admin key")

	// The failures, in a window of their own. Everything they redeem is
	// confirmed now, so no code has outlived its 60 seconds.
	time.Sleep(time.Until(started.Add(authFailureWindow + time.Second)))
	verifierB, verifierC, verifierE := cliRandomToken(), cliRandomToken(), cliRandomToken()
	codeB := s.startConfirmed(b, verifierB, fmt.Sprintf("e5-it-b-%d", stamp))
	expiredHint := fmt.Sprintf("e5-it-c-%d", stamp)
	codeC := s.startConfirmed(b, verifierC, expiredHint)
	// Precondition only: age C's code past its deadline.
	s.Require().NoError(f.conn.Model(&models.CLILoginTransaction{}).
		Where("hostname_hint = ?", expiredHint).
		Update("redeem_by", time.Now().UTC().Add(-time.Minute)).Error)
	codeE := s.startConfirmed(b, verifierE, fmt.Sprintf("e5-it-e-%d", stamp))
	dq := url.Values{"state": {cliRandomToken()}, "challenge": {cliS256(cliRandomToken())}, "mode": {"manual"}}
	stateD := s.followStart(s.caesiumURL + "/auth/cli/start?" + dq.Encode())

	noCSRF := s.cliHTTP(http.MethodPost, "/auth/cli/authorize", "", b.cookie, "", `{"state":"`+stateD+`"}`)
	s.Equal(http.StatusForbidden, noCSRF.status, "a confirmation without the CSRF token is refused: %s", noCSRF.body)
	page := s.cliHTTP(http.MethodGet, "/auth/cli/finish?state="+stateD, "", b.cookie, "", "")
	s.Equal(http.StatusOK, page.status, "the refused confirmation consumed nothing")
	for _, tc := range []struct {
		name, code, verifier string
	}{
		{"a redeemed code is single use", codeA, verifierA},
		{"an unknown code", cliRandomToken(), verifierA},
		{"an expired code", codeC, verifierC},
		{"a wrong verifier", codeB, cliRandomToken()},
		{"the right verifier after a wrong one burned the code", codeB, verifierB},
	} {
		res := s.exchangeCLI(tc.code, tc.verifier)
		s.Equal(http.StatusUnauthorized, res.status, "%s: %s", tc.name, res.body)
		s.NotContains(res.body, "csk_user_", tc.name)
		s.Contains(res.body, "invalid or expired login code", tc.name)
	}
	status, _ = s.requestWithKey(http.MethodGet, "/auth/whoami", keyA, nil)
	s.Equal(http.StatusUnauthorized, status, "the logged-out key is dead")

	// Log out everywhere voids a code confirmed before it.
	status, body := s.requestWithKey(http.MethodPost, "/v1/auth/users/"+b.user.ID.String()+"/sessions/revoke", s.authAPIKey, nil)
	s.Require().Equal(http.StatusOK, status, body)
	res := s.exchangeCLI(codeE, verifierE)
	s.Equal(http.StatusUnauthorized, res.status, "a code confirmed before log out everywhere mints nothing: %s", res.body)
	s.Zero(s.activeUserKeys(b.user.ID).UserKeysTotal, "no refused exchange minted a key")

	// The wrong verifier was refused as such (not as an expired code).
	status, body = s.requestWithKey(http.MethodGet, "/v1/auth/audit?action=auth.cli_login&actor="+url.QueryEscape(b.user.Email)+"&limit=50", s.authAPIKey, nil)
	s.Require().Equal(http.StatusOK, status, body)
	s.Contains(body, "verifier_mismatch", "the burned code's audit row names the wrong verifier")

	// Leave the next scenario a clean window.
	time.Sleep(authFailureWindow + time.Second)
}
