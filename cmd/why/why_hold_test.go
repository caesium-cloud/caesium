package why

import (
	"testing"

	"github.com/stretchr/testify/require"
)

const holdSkippedBody = `{
  "taskName": "read", "verdict": "SKIPPED", "status": "skipped",
  "skipReason": "dataset_hold:/orders hold=h-1",
  "summary": "SKIPPED — dataset /orders held since run r-1 (assertion rowCount.min: observed 12 vs bound 1000; hold h-1)",
  "trigger": {"type": "cron", "hold": {"disposition": "skipped", "holds": [
    {"holdId": "h-1", "datasetNamespace": "", "dataset": "orders", "heldSinceRunId": "r-1", "detail": "recorded",
     "violations": [{"metric": "rowCount", "assertion": "min", "observed": 12, "bound": 1000}]},
    {"holdId": "h-2", "datasetNamespace": "", "dataset": "users", "detail": "withheld"}
  ]}},
  "baseline": {"kind": "none"}
}`

func TestRenderTable_HoldSkippedTaskShowsEveryHold(t *testing.T) {
	out := renderBody(t, holdSkippedBody)

	require.Contains(t, out, "SKIPPED — dataset /orders held since run r-1 (assertion rowCount.min: observed 12 vs bound 1000; hold h-1)")
	require.Contains(t, out, "SKIP-REASON")
	require.Contains(t, out, "dataset_hold:/orders hold=h-1")
	require.Contains(t, out, "Dataset holds (skipped, 2):")
	require.Contains(t, out, "/orders")
	require.Contains(t, out, "rowCount.min observed 12 vs bound 1000")
	require.Contains(t, out, "/users", "a second hold stays in the table")
	require.Contains(t, out, "withheld")
}

func TestRenderTable_BoundWithheldAndUnrecordedRenderHonestly(t *testing.T) {
	out := renderBody(t, `{
      "taskName": "read", "verdict": "SKIPPED", "status": "skipped", "summary": "SKIPPED — x",
      "trigger": {"hold": {"disposition": "skipped", "holds": [
        {"dataset": "orders", "detail": "unrecorded"},
        {"holdId": "h-3", "dataset": "users", "detail": "withheld", "violations": [
          {"metric": "rowCount", "assertion": "deltaFromBaseline", "observed": 10, "boundWithheld": true},
          {"metric": "ts", "assertion": "missing"}]}
      ]}}
    }`)

	require.Contains(t, out, "rowCount.deltaFromBaseline observed 10 vs bound withheld")
	require.Contains(t, out, "ts.missing observed none vs bound none")
	require.Contains(t, out, "unrecorded")
}

func TestRenderTable_NoHoldBlockWithoutAHold(t *testing.T) {
	out := renderBody(t, `{"taskName": "extract", "verdict": "SKIPPED", "status": "skipped", "summary": "SKIPPED — trigger rule \"all_success\" not satisfied", "skipReason": "trigger rule \"all_success\" not satisfied"}`)

	require.Contains(t, out, "SKIP-REASON")
	require.NotContains(t, out, "Dataset holds")
}
