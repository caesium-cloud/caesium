package system

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

type cliRun struct {
	stdout, stderr string
	err            error
}

func runCLI(t *testing.T, args ...string) cliRun {
	t.Helper()
	t.Setenv("CAESIUM_API_KEY", "")
	root := &cobra.Command{Use: "caesium"}
	root.AddCommand(newCommand())
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return cliRun{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

type recordedRequest struct {
	method, path, auth string
}

func fakeServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Pointer[recordedRequest]) {
	t.Helper()
	var last atomic.Pointer[recordedRequest]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last.Store(&recordedRequest{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization")})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &last
}

const removedBody = `{"status":"removed","removed":{"id":"3297041220608546238","address":"10.244.2.5:9001","role":"spare","leader":false,"reachable":false},` +
	`"leader":{"id":"7","address":"10.244.1.9:9001","role":"voter","leader":true},` +
	`"members":[{"id":"7","address":"10.244.1.9:9001","role":"voter","leader":true},{"id":"8","address":"10.244.2.9:9001","role":"voter","leader":false},{"id":"9","address":"10.244.3.9:9001","role":"voter","leader":false}]}`

const refusedBody = `{"status":"refused","id":"8","reason":"voter","reasons":["voter","insufficient_voters"],` +
	`"message":"member is a voter; only a demoted (spare or standby) member may be removed","retryable":false}`

func TestRemoveSendsTheIDAndReportsTheRemoval(t *testing.T) {
	srv, last := fakeServer(t, http.StatusOK, removedBody)
	t.Setenv("CAESIUM_API_KEY", "")
	run := runCLI(t, "system", "nodes", "remove", "3297041220608546238", "--server", srv.URL+"/", "--api-key", "csk_admin")
	require.NoError(t, run.err, run.stderr)
	require.Equal(t, http.MethodDelete, last.Load().method)
	require.Equal(t, "/v1/system/nodes/3297041220608546238", last.Load().path)
	require.Equal(t, "Bearer csk_admin", last.Load().auth)
	require.Contains(t, run.stdout, "Removed dqlite member 3297041220608546238 (10.244.2.5:9001, was spare)")
	require.Contains(t, run.stdout, "3 members")
	require.Contains(t, run.stderr, "--api-key is visible in process listings")
}

func TestRemoveJSONStdoutIsExactlyTheServerAnswer(t *testing.T) {
	srv, _ := fakeServer(t, http.StatusOK, removedBody)
	run := runCLI(t, "system", "nodes", "remove", "3297041220608546238", "--server", srv.URL, "--json")
	require.NoError(t, run.err, run.stderr)
	var body struct {
		Status  string `json:"status"`
		Removed struct {
			ID string `json:"id"`
		} `json:"removed"`
	}
	require.NoError(t, json.Unmarshal([]byte(run.stdout), &body), "stdout must be one JSON object: %q", run.stdout)
	require.Equal(t, "removed", body.Status)
	require.Equal(t, "3297041220608546238", body.Removed.ID)
}

func TestRemoveRefusalExitsNonZeroWithJSONOnStdoutAndTheReasonOnStderr(t *testing.T) {
	srv, _ := fakeServer(t, http.StatusConflict, refusedBody)

	run := runCLI(t, "system", "nodes", "remove", "8", "--server", srv.URL, "--json")
	require.Error(t, run.err)
	require.Contains(t, run.err.Error(), "HTTP 409, voter")
	require.Contains(t, run.stderr, "refused to remove dqlite member 8")
	var body struct {
		Status    string   `json:"status"`
		Reason    string   `json:"reason"`
		Reasons   []string `json:"reasons"`
		Retryable bool     `json:"retryable"`
	}
	require.NoError(t, json.Unmarshal([]byte(run.stdout), &body), "stdout must be one JSON object: %q", run.stdout)
	require.Equal(t, "refused", body.Status)
	require.Equal(t, "voter", body.Reason)
	require.Equal(t, []string{"voter", "insufficient_voters"}, body.Reasons)
	require.NotContains(t, run.stdout, "Usage:", "a refusal is not a usage error")

	run = runCLI(t, "system", "nodes", "remove", "8", "--server", srv.URL)
	require.Error(t, run.err)
	require.Empty(t, run.stdout, "without --json a refusal writes nothing to stdout")
}

func TestRemoveMarksRetryableRefusals(t *testing.T) {
	srv, _ := fakeServer(t, http.StatusConflict,
		`{"status":"refused","id":"8","reason":"configuration_change_in_progress","reasons":["configuration_change_in_progress"],"message":"busy","retryable":true}`)
	run := runCLI(t, "system", "nodes", "remove", "8", "--server", srv.URL)
	require.Error(t, run.err)
	require.Contains(t, run.err.Error(), "retryable")
}

func TestRemoveReportsNonRefusalErrors(t *testing.T) {
	srv, _ := fakeServer(t, http.StatusForbidden, `{"message":"insufficient permissions"}`)
	run := runCLI(t, "system", "nodes", "remove", "8", "--server", srv.URL)
	require.Error(t, run.err)
	require.Contains(t, run.err.Error(), "HTTP 403")
	require.Contains(t, run.err.Error(), "insufficient permissions")
}

func TestRemoveRejectsAnInvalidIDWithoutARequest(t *testing.T) {
	srv, last := fakeServer(t, http.StatusOK, removedBody)
	for _, raw := range []string{"abc", "0", "-1", "18446744073709551616"} {
		run := runCLI(t, "system", "nodes", "remove", "--server", srv.URL, "--", raw)
		require.Error(t, run.err, raw)
		require.Contains(t, run.err.Error(), "is not a dqlite node ID")
		require.Empty(t, run.stdout)
	}
	require.Nil(t, last.Load(), "an invalid ID must not reach the server")
}

func TestListPrintsIDsForRaftMembers(t *testing.T) {
	body := `[{"id":"3297041220608546238","address":"10.244.2.5:9001","role":"spare","leader":false,"reachability":"unreachable"},` +
		`{"id":"7","address":"10.244.1.9:9001","role":"voter","leader":true,"reachability":"reachable"},` +
		`{"address":"10.0.0.99:9001","role":"worker","leader":false,"reachability":"unknown"}]`
	srv, last := fakeServer(t, http.StatusOK, body)

	run := runCLI(t, "system", "nodes", "list", "--server", srv.URL)
	require.NoError(t, run.err, run.stderr)
	require.Equal(t, "/v1/system/nodes", last.Load().path)
	lines := strings.Split(strings.TrimSpace(run.stdout), "\n")
	require.Len(t, lines, 4)
	require.Equal(t, []string{"ID", "ADDRESS", "ROLE", "REACHABILITY", "LEADER"}, strings.Fields(lines[0]))
	require.Equal(t, []string{"3297041220608546238", "10.244.2.5:9001", "spare", "unreachable", "false"}, strings.Fields(lines[1]))
	require.Equal(t, []string{"-", "10.0.0.99:9001", "worker", "unknown", "false"}, strings.Fields(lines[3]))

	run = runCLI(t, "system", "nodes", "list", "--server", srv.URL, "--json")
	require.NoError(t, run.err, run.stderr)
	var nodes []map[string]any
	require.NoError(t, json.Unmarshal([]byte(run.stdout), &nodes))
	require.Len(t, nodes, 3)
	require.Equal(t, "3297041220608546238", nodes[0]["id"])
}
