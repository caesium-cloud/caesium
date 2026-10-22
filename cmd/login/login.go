// Package login holds `caesium login`, `caesium logout` and `caesium whoami`
// (identity-and-access E5, design §13): interactive CLI sign-in through the
// server's configured SSO provider, the credential store it writes, and the
// principal the CLI acts as.
package login

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/caesium-cloud/caesium/internal/clihttp"
	"github.com/spf13/cobra"
)

const defaultServer = "http://localhost:8080"

var (
	loginServer    string
	loginNoBrowser bool
	loginNoOpen    bool
	loginCode      string
	loginTimeout   time.Duration
)

// openBrowser launches the system browser at target; tests replace it.
var openBrowser = func(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
}

// LoginCmd is `caesium login`.
var LoginCmd = &cobra.Command{
	Use:   "login",
	Args:  cobra.NoArgs,
	Short: "Sign the CLI in through the server's sign-in provider",
	Long: "Sign the CLI in as your SSO user. By default the CLI opens a browser at the " +
		"server, you confirm the sign-in there, and the server hands the CLI a user-bound " +
		"key through a listener on 127.0.0.1. --no-open prints the URL instead of opening a " +
		"browser. --no-browser is for a machine without a browser: it prints a URL to open " +
		"anywhere, the confirmation page shows a code, and `caesium login --code <code>` " +
		"finishes. The key is stored in $XDG_CONFIG_HOME/caesium/credentials.yaml (0600) for " +
		"the server and used by every command when neither --api-key nor CAESIUM_API_KEY is set.",
	RunE: runLogin,
}

func runLogin(cmd *cobra.Command, _ []string) error {
	server, err := cliutil.CanonicalServer(loginServer)
	if err != nil {
		return err
	}
	stderr := cmd.ErrOrStderr()
	if strings.TrimSpace(os.Getenv(cliutil.APIKeyEnvVar)) != "" {
		_, _ = fmt.Fprintf(stderr, "warning: %s is set and takes precedence over the stored login\n", cliutil.APIKeyEnvVar)
	}
	if loginCode != "" {
		if loginNoBrowser || loginNoOpen {
			return errors.New("--code cannot be combined with --no-browser or --no-open")
		}
		return redeemPendingCode(cmd, server, loginCode)
	}
	if loginNoBrowser && loginNoOpen {
		return errors.New("--no-browser and --no-open cannot be combined")
	}

	verifier, err := randomToken()
	if err != nil {
		return err
	}
	state, err := randomToken()
	if err != nil {
		return err
	}
	if loginNoBrowser {
		return startManualLogin(cmd, server, state, verifier)
	}
	return loopbackLogin(cmd, server, state, verifier)
}

// randomToken is 32 random bytes, base64url without padding (43 characters):
// a valid RFC 7636 verifier and a valid state.
func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// s256 is the PKCE S256 challenge of verifier (RFC 7636 §4.2).
func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func hostnameHint() string {
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	if len(host) > 64 {
		host = host[:64]
	}
	return host
}

func startURL(server, state, verifier string, port int) string {
	q := url.Values{}
	q.Set("state", state)
	q.Set("challenge", s256(verifier))
	if port > 0 {
		q.Set("port", strconv.Itoa(port))
	} else {
		q.Set("mode", "manual")
	}
	if host := hostnameHint(); host != "" {
		q.Set("hostname", host)
	}
	return server + "/auth/cli/start?" + q.Encode()
}

func startManualLogin(cmd *cobra.Command, server, state, verifier string) error {
	now := time.Now()
	if err := cliutil.UpdateCredentials(func(store *cliutil.CredentialStore) error {
		return store.PutPending(cliutil.PendingLogin{
			Server:    server,
			State:     state,
			Verifier:  verifier,
			ExpiresAt: now.Add(cliutil.PendingLoginTTL),
		}, now)
	}); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), startURL(server, state, verifier, 0))
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
		"Open the URL above in a browser signed in to Caesium and authorise the CLI.\n"+
			"Then, within %s, run: caesium login --server %s --code <code>\n",
		cliutil.PendingLoginTTL, server)
	return nil
}

func redeemPendingCode(cmd *cobra.Command, server, code string) error {
	now := time.Now()
	explicit := false
	if flag := cmd.Flags().Lookup("server"); flag != nil && flag.Changed {
		explicit = true
	}
	var verifier string
	// The pending entry is single use whatever the exchange answers, so it is
	// taken (and the file saved) before the exchange.
	if err := cliutil.UpdateCredentials(func(store *cliutil.CredentialStore) error {
		// `caesium login --code <c>` without --server (what the confirmation
		// page shows) redeems the one pending login, whichever server it is
		// for.
		if !explicit {
			switch servers := store.PendingServers(now); len(servers) {
			case 1:
				server = servers[0]
			case 0:
			default:
				return fmt.Errorf("pending logins for several servers (%s); pass --server", strings.Join(servers, ", "))
			}
		}
		pending, ok := store.TakePending(server, now)
		if !ok {
			return fmt.Errorf("no pending login for %s; run caesium login --no-browser --server %s first", server, server)
		}
		verifier = pending.Verifier
		return nil
	}); err != nil {
		return err
	}
	return exchangeAndStore(cmd, server, strings.TrimSpace(code), verifier)
}

type callbackResult struct {
	code string
}

func loopbackLogin(cmd *cobra.Command, server, state, verifier string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("start the loopback listener: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	results := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 || q.Get("code") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("<!doctype html><title>Caesium CLI</title><p>This sign-in callback does not match the running caesium login.</p>"))
			return
		}
		_, _ = w.Write([]byte("<!doctype html><title>Caesium CLI</title><p>Signed in. You can close this window and return to the terminal.</p>"))
		select {
		case results <- callbackResult{code: q.Get("code")}:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(listener) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	target := startURL(server, state, verifier, port)
	if loginNoOpen {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), target)
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Open the URL above in a browser on this computer and authorise the CLI.")
	} else if err := openBrowser(target); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Could not open a browser (%v). Open this URL on this computer:\n%s\n", err, target)
	} else {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Opened a browser to sign in. If it did not open, visit:\n%s\n", target)
	}

	timer := time.NewTimer(loginTimeout)
	defer timer.Stop()
	select {
	case res := <-results:
		return exchangeAndStore(cmd, server, res.code, verifier)
	case <-timer.C:
		return fmt.Errorf("timed out after %s waiting for the browser sign-in", loginTimeout)
	case <-ctx.Done():
		return ctx.Err()
	}
}

type exchangeResponse struct {
	Key       string    `json:"key"`
	KeyID     string    `json:"key_id"`
	KeyPrefix string    `json:"key_prefix"`
	ExpiresAt time.Time `json:"expires_at"`
	Subject   string    `json:"subject"`
}

func exchangeAndStore(cmd *cobra.Command, server, code, verifier string) error {
	payload, err := json.Marshal(map[string]string{"code": code, "code_verifier": verifier})
	if err != nil {
		return err
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	body, status, readErr := clihttp.Exchange(cmd.Context(), boundedClient(), http.MethodPost, server+"/auth/cli/exchange", bytes.NewReader(payload), headers)
	if err := clihttp.ResponseError("login", status, body, readErr); err != nil {
		return err
	}
	var resp exchangeResponse
	if err := json.Unmarshal(body, &resp); err != nil || resp.Key == "" {
		return errors.New("login: the server's exchange response carried no key")
	}
	var path string
	if err := cliutil.UpdateCredentials(func(store *cliutil.CredentialStore) error {
		path = store.Path()
		return store.Put(cliutil.StoredCredential{
			Server:    server,
			Key:       resp.Key,
			KeyID:     resp.KeyID,
			KeyPrefix: resp.KeyPrefix,
			ExpiresAt: resp.ExpiresAt,
			Subject:   resp.Subject,
		})
	}); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Logged in to %s as %s (key %s…, expires %s). Saved to %s.\n",
		server, resp.Subject, resp.KeyPrefix, resp.ExpiresAt.Local().Format(time.RFC1123), path)
	return nil
}

// boundedClient is http.DefaultClient (so tests can replace its transport)
// with an overall deadline: a server that accepts the connection and then
// stalls must not hang login, logout or whoami.
func boundedClient() *http.Client {
	c := *http.DefaultClient
	if c.Timeout == 0 || c.Timeout > cliutil.DefaultHTTPTimeout {
		c.Timeout = cliutil.DefaultHTTPTimeout
	}
	return &c
}

func init() {
	LoginCmd.Flags().StringVar(&loginServer, "server", defaultServer, "Caesium server base URL")
	LoginCmd.Flags().BoolVar(&loginNoBrowser, "no-browser", false, "Print a sign-in URL and finish with --code (for a machine without a browser)")
	LoginCmd.Flags().BoolVar(&loginNoOpen, "no-open", false, "Print the sign-in URL instead of opening a browser (the callback still returns to this computer)")
	LoginCmd.Flags().StringVar(&loginCode, "code", "", "Finish a --no-browser login with the code the confirmation page showed")
	LoginCmd.Flags().DurationVar(&loginTimeout, "timeout", 5*time.Minute, "How long to wait for the browser sign-in")
}
