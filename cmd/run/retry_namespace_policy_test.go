package run

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// usePolicyFileEnv sets CAESIUM_AUTH_POLICY_FILE for the test. The file is
// never read: the command refuses on the setting alone.
func usePolicyFileEnv(t *testing.T) {
	t.Helper()
	// Registered before Setenv so it runs after Setenv's restore (LIFO).
	t.Cleanup(func() { _ = env.Process() })
	t.Setenv("CAESIUM_AUTH_POLICY_FILE", filepath.Join(t.TempDir(), "policy.yaml"))
	require.NoError(t, env.Process())
}

// clearServerFlag makes the command take the direct path: pflag never resets
// Changed, so an earlier --server test would route it over HTTP.
func clearServerFlag(t *testing.T) {
	t.Helper()
	server := retryCmd.Flags().Lookup("server")
	changed := server.Changed
	server.Changed = false
	t.Cleanup(func() { server.Changed = changed })
}

// TestDirectRetryRefusesUnderAConfiguredPolicyFile is the regression for the
// no-server `caesium run retry`: it reopened the run straight against the
// configured catalog with the namespace gate open, because only `caesium start`
// installs the policy the gate reads — so against a shared catalog it could
// reopen a run past its namespace's quota or in an undeclared namespace. With
// CAESIUM_AUTH_POLICY_FILE set the real command now refuses before the store
// is touched (the job lookup would open the catalog) and loads no policy.
func TestDirectRetryRefusesUnderAConfiguredPolicyFile(t *testing.T) {
	restoreRetryTestGlobals(t)
	usePolicyFileEnv(t)
	clearServerFlag(t)
	previous := policy.ProcessHolder()
	t.Cleanup(func() { policy.SetProcessHolder(previous) })
	policy.SetProcessHolder(nil)

	stdout, stderr, err := executeRunCommand(t, "retry",
		"--job-id", uuid.NewString(), "--run-id", uuid.NewString())
	require.ErrorIs(t, err, errDirectRetryUnderPolicy)
	require.Contains(t, err.Error(), "--server")
	require.NotContains(t, stdout+stderr, "Usage:", "a refusal is not a usage error")
	require.Nil(t, policy.ProcessHolder(),
		"the client loads no policy: its secret resolution stays client-side")
}

// TestRetryOverServerIsNotRefusedUnderAPolicyFile: the policy setting only
// closes the direct path; --server is the way through.
func TestRetryOverServerIsNotRefusedUnderAPolicyFile(t *testing.T) {
	restoreRetryTestGlobals(t)
	usePolicyFileEnv(t)
	jobID, runID := uuid.NewString(), uuid.NewString()
	var sawPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"` + runID + `","job_id":"` + jobID + `","status":"pending"}`))
	}))
	defer server.Close()

	_, stderr, err := executeRunCommand(t, "retry", "--job-id", jobID, "--run-id", runID, "--server", server.URL)
	require.NoError(t, err, "stderr=%s", stderr)
	require.Equal(t, "/v1/jobs/"+jobID+"/runs/"+runID+"/retry", sawPath)
}

// TestDirectRetryWithoutAPolicyFileIsUnchanged: no file, no refusal.
func TestDirectRetryWithoutAPolicyFileIsUnchanged(t *testing.T) {
	require.NoError(t, refuseDirectRetryUnderPolicy(env.Environment{}))
	require.NoError(t, refuseDirectRetryUnderPolicy(env.Environment{AuthPolicyFile: "  "}))
	require.ErrorIs(t, refuseDirectRetryUnderPolicy(env.Environment{AuthPolicyFile: "/etc/caesium/policy.yaml"}), errDirectRetryUnderPolicy)
}
