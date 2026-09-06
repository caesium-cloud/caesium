package run

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// restoreRetryCallbacksGlobals restores the package-level flag targets the
// retry-callbacks command binds, so cobra's shared command object cannot leak
// state between tests.
func restoreRetryCallbacksGlobals(t *testing.T) {
	t.Helper()
	originalJobID := retryJobID
	originalRunID := retryRunID
	originalServer := retryCallbacksServer
	originalAPIKey := retryCallbacksAPIKey
	t.Cleanup(func() {
		retryJobID = originalJobID
		retryRunID = originalRunID
		retryCallbacksServer = originalServer
		retryCallbacksAPIKey = originalAPIKey
	})
}

// TestRetryCallbacksOverServerPostsToTheCallbackRetryRoute pins the transport
// half of the --server path: without it the command could only reach the
// in-process store, which opens a NATIVE dqlite node on CAESIUM_NODE_ADDRESS
// and so cannot run beside a server that already holds that address.
func TestRetryCallbacksOverServerPostsToTheCallbackRetryRoute(t *testing.T) {
	restoreRetryCallbacksGlobals(t)

	var sawMethod, sawPath, sawAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawMethod = r.Method
		sawPath = r.URL.Path
		sawAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"6f1c9d6e-0000-4000-8000-000000000002"}`))
	}))
	defer server.Close()

	t.Setenv(runDiffAPIKeyEnvVar, "env-key")

	stdout, stderr, err := executeRunCommand(t, "retry-callbacks",
		"--job-id", "6f1c9d6e-0000-4000-8000-000000000001",
		"--run-id", "6f1c9d6e-0000-4000-8000-000000000002",
		"--server", server.URL)

	require.NoError(t, err, "stderr: %s", stderr)
	require.Equal(t, http.MethodPost, sawMethod)
	require.Equal(t,
		"/v1/jobs/6f1c9d6e-0000-4000-8000-000000000001/runs/6f1c9d6e-0000-4000-8000-000000000002/callbacks/retry",
		sawPath)
	require.Equal(t, "Bearer env-key", sawAuth,
		"the CAESIUM_API_KEY fallback must apply here as it does for `run retry`")
	require.Contains(t, stdout,
		"Retried failed callbacks for run 6f1c9d6e-0000-4000-8000-000000000002",
		"the success line must stay byte-identical to the in-process path")
}

// TestRetryCallbacksOverServerSurfacesServerErrorNoUsageBlock keeps a server
// error out of the "usage" path: cobra prints the whole flag help after ANY
// RunE error unless SilenceUsage is flipped, which buries the actionable line.
func TestRetryCallbacksOverServerSurfacesServerErrorNoUsageBlock(t *testing.T) {
	restoreRetryCallbacksGlobals(t)

	const notFoundMsg = "run not found"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"` + notFoundMsg + `"}`))
	}))
	defer server.Close()

	stdout, stderr, err := executeRunCommand(t, "retry-callbacks",
		"--job-id", "6f1c9d6e-0000-4000-8000-000000000003",
		"--run-id", "6f1c9d6e-0000-4000-8000-000000000004",
		"--server", server.URL)

	require.Error(t, err)
	require.Contains(t, stderr, notFoundMsg)
	require.NotContains(t, stderr, "Usage:", "usage must not follow a server error:\n%s", stderr)
	require.Empty(t, stdout, "a failed retry writes nothing to stdout:\n%s", stdout)
}

// TestRetryCallbacksRejectsMalformedIDsBeforeAnyTransport proves argument
// validation still produces a usage error (SilenceUsage is flipped only after
// the ids parse), and that it never reaches the network.
func TestRetryCallbacksRejectsMalformedIDsBeforeAnyTransport(t *testing.T) {
	restoreRetryCallbacksGlobals(t)

	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	_, _, err := executeRunCommand(t, "retry-callbacks",
		"--job-id", "not-a-uuid",
		"--run-id", "6f1c9d6e-0000-4000-8000-000000000004",
		"--server", server.URL)

	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid job id")
	require.False(t, called, "a malformed id must never reach the server")
}
