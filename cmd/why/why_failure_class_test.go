package why

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRenderTable_FailureClassOnlyWhenPresent: a failed task's class is shown,
// for the task and for a fanned group's first failure; an explanation without
// one renders no class row.
func TestRenderTable_FailureClassOnlyWhenPresent(t *testing.T) {
	out := renderBody(t, `{"taskName": "use", "verdict": "EXECUTED", "status": "failed", "failureClass": "config",
      "summary": "failed", "trigger": {"type": "manual"}}`)
	require.Contains(t, out, "FAILURE-CLASS")
	require.Contains(t, out, "config")

	out = renderBody(t, `{"taskName": "use", "verdict": "EXECUTED", "status": "failed", "summary": "failed"}`)
	require.NotContains(t, out, "FAILURE-CLASS")

	out = renderBody(t, `{"taskName": "fan", "summary": "group", "group": {"partitionCount": 2,
      "statusCounts": {"failed": 1, "succeeded": 1}, "partitions": ["a", "b"],
      "firstFailure": {"partition": "b", "partitionIndex": 1, "status": "failed", "attempt": 1, "error": "denied", "failureClass": "config"}}}`)
	require.Contains(t, out, "Failure class: config")
	require.Contains(t, out, `First failure: partition "b"`)
}
