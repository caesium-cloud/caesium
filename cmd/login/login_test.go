package login

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

const (
	testCode = "the-one-time-code-the-one-time-code-the-one"
	testKey  = "csk_user_testkeytestkeytestkey"
)

// fakeServer answers the exchange (checking the verifier against the
// challenge the start URL carried), logout and whoami.
type fakeServer struct {
	*httptest.Server
	mu         sync.Mutex
	challenge  string
	logouts    []string
	logoutCode int
}

func newFakeServer(t *testing.T) *fakeServer {
	return newFakeServerAt(t, "")
}

// newFakeServerAt serves the fake under the decoded path prefix (e.g.
// "/team#blue", reached as /team%23blue); requests outside it get 404.
func newFakeServerAt(t *testing.T, prefix string) *fakeServer {
	f := &fakeServer{logoutCode: http.StatusNoContent}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/cli/exchange", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		challenge := f.challenge
		f.mu.Unlock()
		if body["code"] != testCode || s256(body["code_verifier"]) != challenge {
			http.Error(w, `{"message":"invalid or expired login code"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"key": testKey, "key_id": "kid", "key_prefix": testKey[:13],
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "subject": "mia@example.com",
		})
	})
	mux.HandleFunc("/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.logouts = append(f.logouts, r.Header.Get("Authorization"))
		code := f.logoutCode
		f.mu.Unlock()
		w.WriteHeader(code)
	})
	mux.HandleFunc("/auth/whoami", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			http.Error(w, `{"message":"missing credentials"}`, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"kind":"user","subject":"mia@example.com","cluster_role":"","grants":{"marketing":"operator"},"namespaces":["marketing"],"is_scoped":false}`))
	})
	var handler http.Handler = mux
	if prefix != "" {
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rest, ok := strings.CutPrefix(r.URL.Path, prefix)
			if !ok || !strings.HasPrefix(rest, "/") {
				http.NotFound(w, r)
				return
			}
			r2 := r.Clone(r.Context())
			r2.URL.Path, r2.URL.RawPath = rest, ""
			mux.ServeHTTP(w, r2)
		})
	}
	f.Server = httptest.NewServer(handler)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeServer) recordStart(t *testing.T, start string) url.Values {
	t.Helper()
	u, err := url.Parse(start)
	require.NoError(t, err)
	require.Equal(t, "/auth/cli/start", u.Path)
	q := u.Query()
	require.Len(t, q.Get("state"), 43)
	require.Len(t, q.Get("challenge"), 43)
	f.mu.Lock()
	f.challenge = q.Get("challenge")
	f.mu.Unlock()
	return q
}

func getURL(target string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

func testCommand() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	cmd := &cobra.Command{Use: "test"}
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetContext(context.Background())
	return cmd, &stdout, &stderr
}

func resetFlags(t *testing.T, server string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(cliutil.APIKeyEnvVar, "")
	loginServer, loginNoBrowser, loginNoOpen, loginCode, loginTimeout = server, false, false, "", 10*time.Second
	logoutServer = server
	whoamiServer, whoamiAPIKey, whoamiJSON = server, "", false
}

func storedKey(t *testing.T, server string) (string, bool) {
	t.Helper()
	store, err := cliutil.LoadCredentials()
	require.NoError(t, err)
	cred, ok := store.Lookup(server)
	if !ok {
		return "", false
	}
	return cred.Key, true
}

func TestLoopbackLoginVerifiesStateAndStoresTheKey(t *testing.T) {
	srv := newFakeServer(t)
	resetFlags(t, srv.URL)
	callbackErr := make(chan error, 1)
	openBrowser = func(target string) error {
		q := srv.recordStart(t, target)
		port := q.Get("port")
		require.NotEmpty(t, port)
		require.Empty(t, q.Get("mode"))
		go func() {
			base := "http://127.0.0.1:" + port + "/callback?"
			resp, err := getURL(base + url.Values{"state": {"wrong"}, "code": {testCode}}.Encode())
			if err != nil {
				callbackErr <- err
				return
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				callbackErr <- errors.New("a callback with the wrong state was accepted")
				return
			}
			resp, err = getURL(base + url.Values{"state": {q.Get("state")}, "code": {testCode}}.Encode())
			if err == nil {
				_ = resp.Body.Close()
			}
			callbackErr <- err
		}()
		return nil
	}
	t.Cleanup(func() { openBrowser = nil })

	cmd, stdout, stderr := testCommand()
	require.NoError(t, runLogin(cmd, nil))
	require.NoError(t, <-callbackErr)
	require.Empty(t, stdout.String(), "the default flow prints nothing on stdout")
	require.Contains(t, stderr.String(), "Logged in to "+srv.URL+" as mia@example.com")
	require.NotContains(t, stderr.String(), testKey, "the key is never printed")
	key, ok := storedKey(t, srv.URL)
	require.True(t, ok)
	require.Equal(t, testKey, key)
}

func TestNoOpenPrintsTheURLOnStdout(t *testing.T) {
	srv := newFakeServer(t)
	resetFlags(t, srv.URL)
	loginNoOpen = true
	openBrowser = func(string) error { t.Fatal("--no-open must not open a browser"); return nil }
	t.Cleanup(func() { openBrowser = nil })
	loginTimeout = 200 * time.Millisecond

	cmd, stdout, _ := testCommand()
	err := runLogin(cmd, nil)
	require.ErrorContains(t, err, "timed out")
	srv.recordStart(t, strings.TrimSpace(stdout.String()))
	_, ok := storedKey(t, srv.URL)
	require.False(t, ok)
}

func TestNoBrowserThenCodeLogsIn(t *testing.T) {
	srv := newFakeServer(t)
	resetFlags(t, srv.URL)
	loginNoBrowser = true
	cmd, stdout, stderr := testCommand()
	require.NoError(t, runLogin(cmd, nil))
	q := srv.recordStart(t, strings.TrimSpace(stdout.String()))
	require.Equal(t, "manual", q.Get("mode"))
	require.Empty(t, q.Get("port"))
	require.Contains(t, stderr.String(), "caesium login --server "+srv.URL+" --code")

	resetLoginOnly := func() { loginNoBrowser, loginCode = false, testCode }
	resetLoginOnly()
	cmd, _, _ = testCommand()
	require.NoError(t, runLogin(cmd, nil))
	key, ok := storedKey(t, srv.URL)
	require.True(t, ok)
	require.Equal(t, testKey, key)

	cmd, _, _ = testCommand()
	require.ErrorContains(t, runLogin(cmd, nil), "no pending login", "the pending verifier is single use")
}

// A server mounted under an escaped path keeps its escapes in every request
// login, whoami and logout send: %23 must not become a fragment that swallows
// the endpoint.
func TestLoginWhoamiLogoutUnderAnEscapedServerPath(t *testing.T) {
	srv := newFakeServerAt(t, "/team#blue")
	server := srv.URL + "/team%23blue"
	resetFlags(t, server+"/")
	loginNoBrowser = true
	cmd, stdout, _ := testCommand()
	require.NoError(t, runLogin(cmd, nil))
	start, err := url.Parse(strings.TrimSpace(stdout.String()))
	require.NoError(t, err)
	require.Equal(t, "/team%23blue/auth/cli/start", start.EscapedPath())
	require.Empty(t, start.Fragment)
	srv.mu.Lock()
	srv.challenge = start.Query().Get("challenge")
	srv.mu.Unlock()

	loginNoBrowser, loginCode = false, testCode
	cmd, _, stderr := testCommand()
	require.NoError(t, runLogin(cmd, nil))
	require.Contains(t, stderr.String(), "Logged in to "+server+" as mia@example.com")
	key, ok := storedKey(t, server)
	require.True(t, ok)
	require.Equal(t, testKey, key)

	cmd, stdout, _ = testCommand()
	cmd.Flags().String("server", server+"/", "")
	require.NoError(t, WhoamiCmd.RunE(cmd, nil))
	require.Contains(t, stdout.String(), "mia@example.com")

	cmd, _, _ = testCommand()
	require.NoError(t, LogoutCmd.RunE(cmd, nil))
	require.Equal(t, []string{"Bearer " + testKey}, srv.logouts)
}

// A key the exchange minted but the CLI could not save is revoked with
// itself as the bearer, so no active key is left that logout cannot reach;
// both outcomes are reported, never the key.
func TestFailedSaveRevokesTheExchangedKey(t *testing.T) {
	srv := newFakeServer(t)
	resetFlags(t, srv.URL)
	// $XDG_CONFIG_HOME/caesium is a regular file: the store cannot be created.
	xdg := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(xdg, "caesium"), []byte("not a directory"), 0o600))
	t.Setenv("XDG_CONFIG_HOME", xdg)
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	srv.mu.Lock()
	srv.challenge = s256(verifier)
	srv.mu.Unlock()

	cmd, stdout, stderr := testCommand()
	err := exchangeAndStore(cmd, srv.URL, testCode, verifier)
	require.ErrorContains(t, err, "could not save the new key, so it was revoked on the server")
	require.ErrorContains(t, err, "create credentials directory")
	require.NotContains(t, err.Error(), testKey, "the key is never printed")
	require.NotContains(t, stdout.String()+stderr.String(), testKey)
	require.Equal(t, []string{"Bearer " + testKey}, srv.logouts, "the unsaved key revokes itself")

	srv.logoutCode = http.StatusInternalServerError
	cmd, stdout, stderr = testCommand()
	err = exchangeAndStore(cmd, srv.URL, testCode, verifier)
	require.ErrorContains(t, err, "create credentials directory")
	require.ErrorContains(t, err, "revoking it on the server also failed")
	require.ErrorContains(t, err, "logout failed (500)")
	require.ErrorContains(t, err, testKey[:13]+"…", "the error names the key by its prefix")
	require.NotContains(t, err.Error(), testKey, "the key is never printed")
	require.NotContains(t, stdout.String()+stderr.String(), testKey)
	require.Len(t, srv.logouts, 2)

	// A cancelled command still gets its bounded revoke attempt.
	srv.logoutCode = http.StatusNoContent
	ctx, cancel := context.WithCancel(context.Background())
	cmd, _, _ = testCommand()
	cmd.SetContext(ctx)
	cancel()
	require.NoError(t, revokeUnsavedKey(cmd, srv.URL, testKey))
	require.Len(t, srv.logouts, 3)
}

// The confirmation page shows `caesium login --code <c>` without --server:
// it redeems the single pending login whatever the default server is, and an
// explicit --server still wins.
func TestCodeWithoutServerRedeemsTheOnePendingLogin(t *testing.T) {
	srv := newFakeServer(t)
	resetFlags(t, srv.URL)
	loginNoBrowser = true
	cmd, stdout, _ := testCommand()
	require.NoError(t, runLogin(cmd, nil))
	srv.recordStart(t, strings.TrimSpace(stdout.String()))

	loginNoBrowser, loginCode, loginServer = false, testCode, defaultServer
	cmd, _, _ = testCommand()
	cmd.Flags().String("server", defaultServer, "")
	require.NoError(t, runLogin(cmd, nil))
	_, ok := storedKey(t, srv.URL)
	require.True(t, ok, "stored under the pending login's server")

	// Two pending logins make an unflagged --code ambiguous.
	store, err := cliutil.LoadCredentials()
	require.NoError(t, err)
	now := time.Now()
	for _, server := range []string{"http://a.example", "http://b.example"} {
		require.NoError(t, store.PutPending(cliutil.PendingLogin{Server: server, State: "s", Verifier: "v", ExpiresAt: now.Add(time.Minute)}, now))
	}
	require.NoError(t, store.Save())
	cmd, _, _ = testCommand()
	cmd.Flags().String("server", defaultServer, "")
	require.ErrorContains(t, runLogin(cmd, nil), "pass --server")
}

func TestCodeWithTheWrongCodeFailsAndConsumesThePending(t *testing.T) {
	srv := newFakeServer(t)
	resetFlags(t, srv.URL)
	loginNoBrowser = true
	cmd, stdout, _ := testCommand()
	require.NoError(t, runLogin(cmd, nil))
	srv.recordStart(t, strings.TrimSpace(stdout.String()))
	loginNoBrowser, loginCode = false, "not-the-code"
	cmd, _, _ = testCommand()
	require.ErrorContains(t, runLogin(cmd, nil), "401")
	_, ok := storedKey(t, srv.URL)
	require.False(t, ok)
	loginCode = testCode
	cmd, _, _ = testCommand()
	require.ErrorContains(t, runLogin(cmd, nil), "no pending login")
}

func TestLoginFlagConflicts(t *testing.T) {
	resetFlags(t, "http://localhost:8080")
	loginNoBrowser, loginNoOpen = true, true
	cmd, _, _ := testCommand()
	require.Error(t, runLogin(cmd, nil))
	loginNoBrowser, loginNoOpen, loginCode = false, true, "x"
	require.Error(t, runLogin(cmd, nil))
	loginServer, loginCode, loginNoOpen = "not a url", "", false
	require.Error(t, runLogin(cmd, nil))
}

func seedLogin(t *testing.T, server string) {
	t.Helper()
	store, err := cliutil.LoadCredentials()
	require.NoError(t, err)
	require.NoError(t, store.Put(cliutil.StoredCredential{Server: server, Key: testKey, ExpiresAt: time.Now().Add(time.Hour)}))
	require.NoError(t, store.Save())
}

func TestWhoamiUsesTheStoredLoginAndKeepsJSONOnStdout(t *testing.T) {
	srv := newFakeServer(t)
	resetFlags(t, srv.URL)
	seedLogin(t, srv.URL)

	cmd, stdout, stderr := testCommand()
	cmd.Flags().String("server", srv.URL, "")
	whoamiJSON = true
	require.NoError(t, WhoamiCmd.RunE(cmd, nil))
	var who map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &who), "stdout is exactly the JSON body")
	require.Equal(t, "mia@example.com", who["subject"])
	require.Empty(t, stderr.String())

	whoamiJSON = false
	cmd, stdout, _ = testCommand()
	cmd.Flags().String("server", srv.URL, "")
	require.NoError(t, WhoamiCmd.RunE(cmd, nil))
	require.Contains(t, stdout.String(), "Subject:      mia@example.com")
	require.Contains(t, stdout.String(), "marketing")
}

func TestLogoutRevokesAndForgetsEvenWhenTheServerFails(t *testing.T) {
	srv := newFakeServer(t)
	resetFlags(t, srv.URL)
	seedLogin(t, srv.URL)
	cmd, _, _ := testCommand()
	require.NoError(t, LogoutCmd.RunE(cmd, nil))
	require.Equal(t, []string{"Bearer " + testKey}, srv.logouts)
	_, ok := storedKey(t, srv.URL)
	require.False(t, ok)

	seedLogin(t, srv.URL)
	srv.logoutCode = http.StatusInternalServerError
	cmd, _, _ = testCommand()
	require.ErrorContains(t, LogoutCmd.RunE(cmd, nil), "did not revoke")
	_, ok = storedKey(t, srv.URL)
	require.False(t, ok, "the local entry is deleted even when the server fails")

	cmd, _, stderr := testCommand()
	require.NoError(t, LogoutCmd.RunE(cmd, nil))
	require.Contains(t, stderr.String(), "Not logged in")
}

// A login that lands while a logout's request is in flight is not deleted by
// that logout, and the bounded client keeps a deadline.
func TestLogoutKeepsALoginThatLandedMeanwhile(t *testing.T) {
	resetFlags(t, "")
	mux := http.NewServeMux()
	var server string
	mux.HandleFunc("/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, cliutil.UpdateCredentials(func(s *cliutil.CredentialStore) error {
			return s.Put(cliutil.StoredCredential{Server: server, Key: "csk_user_newer", ExpiresAt: time.Now().Add(time.Hour)})
		}))
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	server = srv.URL
	logoutServer = srv.URL
	seedLogin(t, srv.URL)

	cmd, _, _ := testCommand()
	require.NoError(t, LogoutCmd.RunE(cmd, nil))
	key, ok := storedKey(t, srv.URL)
	require.True(t, ok)
	require.Equal(t, "csk_user_newer", key)

	require.Equal(t, cliutil.DefaultHTTPTimeout, boundedClient().Timeout)
}

func TestS256MatchesRFC7636AppendixB(t *testing.T) {
	require.Equal(t, "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM", s256("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"))
	v, err := randomToken()
	require.NoError(t, err)
	require.Len(t, v, 43)
}
