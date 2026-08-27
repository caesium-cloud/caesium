package run

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// diff_fanout_test.go covers the E3 renderer half: the server already aligns
// instances by partition VALUE and emits partitionsAdded / partitionsRemoved and
// a per-row `partition`, but `caesium run diff` dropped all three on the floor —
// it printed one row per task name and no partition churn at all, so a fanned
// diff silently showed one instance and hid N-1.

const fannedDiffBody = `{
  "jobId": "job-1",
  "leftRunId": "run-left",
  "rightRunId": "run-right",
  "leftStatus": "succeeded",
  "rightStatus": "succeeded",
  "leftTrigger": {"type": "cron", "alias": "nightly"},
  "rightTrigger": {"type": "cron", "alias": "nightly"},
  "partitionsAdded": ["process:d"],
  "partitionsRemoved": ["process:a"],
  "tasks": [
    {"taskName": "list", "leftStatus": "succeeded", "rightStatus": "succeeded",
     "verdict": "WOULD_CACHE_HIT", "hashEqual": true},
    {"taskName": "process", "partition": "b", "leftStatus": "succeeded", "rightStatus": "succeeded",
     "verdict": "WOULD_CACHE_HIT", "hashEqual": true},
    {"taskName": "process", "partition": "c", "leftStatus": "succeeded", "rightStatus": "failed",
     "verdict": "RERAN", "hashEqual": false,
     "changes": [{"field": "partitionFingerprint", "kind": "scalar", "before": "sha256:aaa", "after": "sha256:bbb"}]}
  ]
}`

func renderFannedDiff(t *testing.T) string {
	t.Helper()
	var diff runDiffResponse
	require.NoError(t, json.Unmarshal([]byte(fannedDiffBody), &diff))

	cmd := &cobra.Command{Use: "test"}
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	renderRunDiffTable(cmd, &diff)
	return stdout.String()
}

func TestRenderRunDiffTable_ReportsPartitionChurn(t *testing.T) {
	out := renderFannedDiff(t)
	require.Contains(t, out, "Partitions added: process:d")
	require.Contains(t, out, "Partitions removed: process:a")
}

func TestRenderRunDiffTable_RendersOneRowPerInstance(t *testing.T) {
	out := renderFannedDiff(t)

	require.Contains(t, out, "PARTITION",
		"a fanned diff needs the partition column or two rows read as duplicates")

	// Both instances of `process` are present in the table and distinguishable
	// by their partition column (the pre-fix renderer emitted one `process` row
	// and hid the other instance entirely).
	lines := strings.Split(out, "\n")
	var processRows []string
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "process" && (fields[1] == "b" || fields[1] == "c") {
			processRows = append(processRows, line)
		}
	}
	require.Len(t, processRows, 2, "each instance is its own row:\n%s", out)
	require.Contains(t, processRows[0], "WOULD_CACHE_HIT")
	require.Contains(t, processRows[1], "RERAN")

	// The per-row change block names the instance, not just the step.
	require.Contains(t, out, "process[c] changes")
	require.Contains(t, out, "partitionFingerprint")
	require.Contains(t, out, "sha256:aaa")
	require.Contains(t, out, "sha256:bbb")
}

// TestRenderRunDiffTable_UnfannedTableUnchanged is the no-regression guard: with
// no partition on any row the header and rows are exactly the pre-fan-out shape
// (no PARTITION column, no partition lines).
func TestRenderRunDiffTable_UnfannedTableUnchanged(t *testing.T) {
	diff := runDiffResponse{
		JobID: "job-1", LeftRunID: "l", RightRunID: "r",
		LeftStatus: "succeeded", RightStatus: "succeeded",
		Tasks: []runDiffTask{
			{TaskName: "extract", LeftStatus: "succeeded", RightStatus: "succeeded", Verdict: "WOULD_CACHE_HIT", HashEqual: true},
		},
	}
	cmd := &cobra.Command{Use: "test"}
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	renderRunDiffTable(cmd, &diff)

	out := stdout.String()
	require.NotContains(t, out, "PARTITION")
	require.NotContains(t, out, "Partitions added")
	require.NotContains(t, out, "Partitions removed")
	require.Contains(t, out, "TASK")
	require.Contains(t, out, "extract")
}

// TestRunDiffCommand_JSONPassesPartitionFieldsThrough proves the machine-readable
// form carries the fan-out fields (it prints the server body re-indented, so the
// guard is that nothing strips or reshapes them) and that stdout stays valid,
// parseable JSON.
func TestRunDiffCommand_JSONPassesPartitionFieldsThrough(t *testing.T) {
	restoreRunDiffTestGlobals(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/jobs/job-1/runs/diff" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(fannedDiffBody))
	}))
	defer server.Close()

	diffJobID = "job-1"
	diffServer = server.URL
	diffAPIKey = ""
	diffJSON = true

	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	require.NoError(t, diffCmd.RunE(cmd, []string{"run-left", "run-right"}))

	require.True(t, json.Valid(stdout.Bytes()), "stdout was not JSON:\n%s", stdout.String())
	require.Empty(t, stderr.String(), "--json output must not be split across streams")

	var decoded runDiffResponse
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &decoded))
	require.Equal(t, []string{"process:d"}, decoded.PartitionsAdded)
	require.Equal(t, []string{"process:a"}, decoded.PartitionsRemoved)
	require.Equal(t, "b", decoded.Tasks[1].Partition)
	require.Equal(t, "c", decoded.Tasks[2].Partition)
}

func restoreRunDiffTestGlobals(t *testing.T) {
	t.Helper()
	originalJobID, originalServer := diffJobID, diffServer
	originalAPIKey, originalJSON := diffAPIKey, diffJSON
	t.Cleanup(func() {
		diffJobID, diffServer = originalJobID, originalServer
		diffAPIKey, diffJSON = originalAPIKey, originalJSON
	})
}
