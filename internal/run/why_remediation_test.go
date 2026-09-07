package run

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestRemediationAppliesTo pins the attribution rule: an approved action is
// attributed to a task only when its RESULT names this run, and — for a
// task-targeted action — this exact task. Over-broad attribution is worse than
// none: it tells an operator "an approved action explains this" when it does
// not, and they stop looking.
func TestRemediationAppliesTo(t *testing.T) {
	runID := uuid.New()
	taskID := uuid.New()
	otherRun := uuid.New()
	otherTask := uuid.New()

	cases := []struct {
		name      string
		result    string
		wantScope string
		wantOK    bool
	}{
		{
			name:      "task-targeted action on this task",
			result:    `{"run_id":"` + runID.String() + `","task_id":"` + taskID.String() + `"}`,
			wantScope: remediationScopeTask,
			wantOK:    true,
		},
		{
			name:      "run-scoped action (no task target) covers every task of the run",
			result:    `{"run_id":"` + runID.String() + `","scope":"one_run"}`,
			wantScope: remediationScopeRun,
			wantOK:    true,
		},
		{
			name:   "task-targeted action on a sibling task is not attributed",
			result: `{"run_id":"` + runID.String() + `","task_id":"` + otherTask.String() + `"}`,
			wantOK: false,
		},
		{
			name:   "action on another run is not attributed",
			result: `{"run_id":"` + otherRun.String() + `","task_id":"` + taskID.String() + `"}`,
			wantOK: false,
		},
		{
			name:   "a result naming no run cannot be attributed",
			result: `{"applied":true}`,
			wantOK: false,
		},
		{
			name:   "an empty result cannot be attributed",
			result: ``,
			wantOK: false,
		},
		{
			name:   "an unparseable result cannot be attributed",
			result: `not json`,
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := approvedRemediationRow{Result: []byte(tc.result)}
			scope, ok := row.appliesTo(runID, taskID)
			require.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				require.Equal(t, tc.wantScope, scope)
			}
		})
	}
}

// TestSummarizeRemediation pins that the decider reaches the FIRST line of
// `caesium why` output, which is what an operator reads.
func TestSummarizeRemediation(t *testing.T) {
	require.Empty(t, summarizeRemediation(nil))

	at := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	line := summarizeRemediation([]WhyRemediation{
		{Type: "skip_task", ApprovedBy: "alice@example.com", ExecutedAt: &at},
	})
	require.Contains(t, line, "skip_task")
	require.Contains(t, line, "alice@example.com")
	require.Contains(t, line, "2026-09-06T12:00:00Z")

	// A decision whose decider was not recorded still renders, rather than
	// producing a dangling "approved by ".
	anonymous := summarizeRemediation([]WhyRemediation{{Type: "override_schema_gate"}})
	require.Contains(t, anonymous, "an operator")
}
