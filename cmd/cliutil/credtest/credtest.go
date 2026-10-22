// Package credtest helps CLI command tests prove a command sends the
// credential cmd/cliutil.ResolveAPIKey resolves (flag, CAESIUM_API_KEY, or
// the stored `caesium login` credential for its --server). Test-only.
package credtest

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/stretchr/testify/require"
)

// Server is the URL the stored login is seeded for; commands under test are
// pointed at it with --server. Requests never leave the process.
const Server = "http://caesium.credtest"

// StoredKey is the seeded stored login's key.
const StoredKey = "csk_user_credtest_stored"

// Recorder captures the Authorization header of every request a command
// sends through http.DefaultClient, answering each with status and body.
type Recorder struct {
	mu   sync.Mutex
	auth []string
}

// Last returns the Authorization header of the most recent request.
func (r *Recorder) Last(t *testing.T) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.auth, "the command sent no request")
	return r.auth[len(r.auth)-1]
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Setup isolates the credential store in a temp XDG_CONFIG_HOME holding a
// stored login for Server, clears CAESIUM_API_KEY, and replaces
// http.DefaultClient for the test with one that records Authorization and
// answers status with body.
func Setup(t *testing.T, status int, body string) *Recorder {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(cliutil.APIKeyEnvVar, "")
	store, err := cliutil.LoadCredentials()
	require.NoError(t, err)
	require.NoError(t, store.Put(cliutil.StoredCredential{Server: Server, Key: StoredKey, ExpiresAt: time.Now().Add(time.Hour)}))
	require.NoError(t, store.Save())

	rec := &Recorder{}
	old := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = old })
	http.DefaultClient = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		rec.mu.Lock()
		rec.auth = append(rec.auth, r.Header.Get("Authorization"))
		rec.mu.Unlock()
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	return rec
}
