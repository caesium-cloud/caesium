package why

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// why_test.go covers the CLI half of E3: the --partition selector reaches the
// server as a query param, the fanned group renders as a group (not as a
// half-filled single-instance table), and an unfanned explanation renders
// exactly the table it always did.

const groupBody = `{
  "runId": "22222222-2222-2222-2222-222222222222",
  "jobId": "11111111-1111-1111-1111-111111111111",
  "taskId": "33333333-3333-3333-3333-333333333333",
  "taskName": "process",
  "verdict": "CACHE_MISS",
  "status": "failed",
  "cacheEnabled": true,
  "summary": "FANNED GROUP — task \"process\" ran 3 partition(s): 1 failed, 2 succeeded; first failure partition \"b\": exit status 1. Re-run with --partition <value> for the per-instance explanation.",
  "trigger": {"type": "cron", "alias": "nightly"},
  "baseline": {"kind": "per_partition"},
  "group": {
    "partitionCount": 3,
    "statusCounts": {"succeeded": 2, "failed": 1},
    "cacheHits": 0,
    "partitions": ["a", "b", "c"],
    "durationMs": 12000,
    "firstFailure": {
      "partition": "b", "partitionIndex": 1,
      "taskRunId": "44444444-4444-4444-4444-444444444444",
      "status": "failed", "attempt": 1, "error": "exit status 1"
    }
  }
}`

const instanceBody = `{
  "runId": "22222222-2222-2222-2222-222222222222",
  "jobId": "11111111-1111-1111-1111-111111111111",
  "taskId": "33333333-3333-3333-3333-333333333333",
  "taskName": "process",
  "taskRunId": "44444444-4444-4444-4444-444444444444",
  "partition": "b",
  "verdict": "CACHE_MISS",
  "status": "failed",
  "cacheEnabled": true,
  "hash": "abc123",
  "summary": "CACHE MISS — ` + "`partition`" + ` changed a→b",
  "trigger": {"type": "cron", "alias": "nightly"},
  "baseline": {"kind": "prior_run", "runId": "55555555-5555-5555-5555-555555555555"},
  "diff": {"hashEqual": false, "changes": [
    {"field": "partitionFingerprint", "kind": "scalar", "before": "sha256:aaa", "after": "sha256:bbb"}
  ]}
}`

func renderBody(t *testing.T, body string) string {
	t.Helper()
	var exp explanation
	require.NoError(t, json.Unmarshal([]byte(body), &exp))

	cmd := &cobra.Command{Use: "test"}
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	renderTable(cmd, &exp)
	return stdout.String()
}

func TestRenderTable_FannedGroupRendersTheGroup(t *testing.T) {
	out := renderBody(t, groupBody)

	require.Contains(t, out, "FANNED GROUP")
	require.Contains(t, out, "Fan-out group (3 partitions)")
	require.Contains(t, out, "SUCCEEDED")
	require.Contains(t, out, "FAILED")
	require.Contains(t, out, "PARTITIONS")
	require.Contains(t, out, "a, b, c")
	require.Contains(t, out, "DURATION")
	require.Contains(t, out, `First failure: partition "b" (index 1, attempt 1): exit status 1`)
	require.Contains(t, out, "--partition <value>",
		"the group answer must name the selector that gets the per-instance one")
	require.NotContains(t, out, "Discriminating fields",
		"a group has no single field diff to show")
}

func TestRenderTable_SelectedInstanceShowsPartitionAndDiff(t *testing.T) {
	out := renderBody(t, instanceBody)

	require.Contains(t, out, "PARTITION")
	require.Contains(t, out, "b")
	require.Contains(t, out, "HASH")
	require.Contains(t, out, "Discriminating fields (1)")
	require.Contains(t, out, "partitionFingerprint")
	require.Contains(t, out, "sha256:aaa")
	require.Contains(t, out, "sha256:bbb")
	require.NotContains(t, out, "Fan-out group")
}

// TestRenderTable_UnfannedOutputUnchanged is the no-regression guard: with no
// partition and no group, the rendered table has neither the PARTITION row nor
// the group block.
func TestRenderTable_UnfannedOutputUnchanged(t *testing.T) {
	body := `{
      "taskName": "extract", "verdict": "CACHE_HIT", "status": "cached", "hash": "abc",
      "summary": "CACHE HIT — every hashed input identical",
      "trigger": {"type": "cron"},
      "baseline": {"kind": "cache_origin", "runId": "r1"},
      "diff": {"hashEqual": true}
    }`
	out := renderBody(t, body)

	require.Contains(t, out, "TASK")
	require.Contains(t, out, "CACHE_HIT")
	require.Contains(t, out, "COMPARED-TO")
	require.Contains(t, out, "All hashed inputs are identical")
	require.NotContains(t, out, "PARTITION")
	require.NotContains(t, out, "Fan-out group")
}

// TestWhyCommand_PartitionFlagReachesTheServer proves the selector is actually
// transmitted (a flag that renders but never leaves the process is the classic
// hollow-green failure) and that --json stdout stays clean and parseable.
func TestWhyCommand_PartitionFlagReachesTheServer(t *testing.T) {
	restoreWhyTestGlobals(t)

	var gotQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(instanceBody))
	}))
	defer server.Close()

	whyJobID = "11111111-1111-1111-1111-111111111111"
	whyTask = "process"
	whyPartition = "b"
	whyServer = server.URL
	whyAPIKey = ""
	whyJSON = true

	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	require.NoError(t, Cmd.RunE(cmd, []string{"22222222-2222-2222-2222-222222222222"}))

	require.Equal(t, "process", gotQuery.Get("task"))
	require.Equal(t, "b", gotQuery.Get("partition"))
	require.True(t, json.Valid(stdout.Bytes()), "--json stdout was not JSON:\n%s", stdout.String())
	require.Empty(t, stderr.String(), "--json output must not be split across streams")
}

// TestWhyCommand_OmittedPartitionIsNotSent keeps the default request identical to
// the pre-fan-out one: no empty partition param on the wire.
func TestWhyCommand_OmittedPartitionIsNotSent(t *testing.T) {
	restoreWhyTestGlobals(t)

	var gotQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(groupBody))
	}))
	defer server.Close()

	whyJobID = "11111111-1111-1111-1111-111111111111"
	whyTask = "process"
	whyPartition = ""
	whyServer = server.URL
	whyAPIKey = ""
	whyJSON = false

	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})

	require.NoError(t, Cmd.RunE(cmd, []string{"22222222-2222-2222-2222-222222222222"}))

	require.False(t, gotQuery.Has("partition"))
	require.Contains(t, stdout.String(), "Fan-out group (3 partitions)")
}

// TestWhyCommand_UnknownPartitionSurfacesTheAvailableValues: the server's 404
// body enumerates the partitions, and the CLI must not swallow it.
func TestWhyCommand_UnknownPartitionSurfacesTheAvailableValues(t *testing.T) {
	restoreWhyTestGlobals(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"run: partition not found: task \"process\" has no partition \"zz\"; available partitions: a, b, c"}`))
	}))
	defer server.Close()

	whyJobID = "11111111-1111-1111-1111-111111111111"
	whyTask = "process"
	whyPartition = "zz"
	whyServer = server.URL
	whyAPIKey = ""
	whyJSON = false

	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	err := Cmd.RunE(cmd, []string{"22222222-2222-2222-2222-222222222222"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "available partitions: a, b, c")
}

func restoreWhyTestGlobals(t *testing.T) {
	t.Helper()
	oJobID, oTask, oPartition := whyJobID, whyTask, whyPartition
	oServer, oAPIKey, oJSON := whyServer, whyAPIKey, whyJSON
	t.Cleanup(func() {
		whyJobID, whyTask, whyPartition = oJobID, oTask, oPartition
		whyServer, whyAPIKey, whyJSON = oServer, oAPIKey, oJSON
	})
}
