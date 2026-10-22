package replay

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/caesium-cloud/caesium/internal/freshness"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
)

// The baseline's freshness views: the start view of the datasets it consumed
// and a derived run's decision view and triggering dataset, all read from the
// namespace it ran in.
var baselineFreshnessParams = map[string]string{
	"mode":                                 "baseline",
	freshness.ConsumedWatermarksStartParam: `{"default-ledger":"42"}`,
	"_consumed_watermarks":                 `{"default-ledger":"42"}`,
	"_derived_from_dataset":                "default-ledger",
	"_trigger_depth":                       "1",
}

// TestReplayIntoAnotherNamespaceDropsTheBaselinesFreshnessViews
// (identity-and-access W7-ε): a replay of a job that moved runs in the job's
// current namespace, so the baseline namespace's dataset views are neither
// persisted on the replay run nor in the descriptors its TaskRuns store, which
// keep their baseline provenance. A replay in the same namespace copies the
// baseline params exactly as before.
func TestReplayIntoAnotherNamespaceDropsTheBaselinesFreshnessViews(t *testing.T) {
	for _, moved := range []bool{false, true} {
		name := "same namespace"
		if moved {
			name = "moved"
		}
		t.Run(name, func(t *testing.T) {
			f := newReplayFixture(t)
			taskID := f.seedTask(t, seedTaskConfig{name: "safe", replaySafe: true, result: "success", output: map[string]string{"val": "42"}})
			require.NoError(t, f.db.Model(&models.JobRun{}).Where("id = ?", f.runID).Update("params", mustJSON(t, baselineFreshnessParams)).Error)
			var row models.TaskRun
			require.NoError(t, f.db.First(&row, "job_run_id = ? AND task_id = ?", f.runID, taskID).Error)
			var descriptor models.TaskExecutionDescriptor
			require.NoError(t, json.Unmarshal(row.ExecutionDescriptor, &descriptor))
			descriptor.Run.Params = baselineFreshnessParams
			require.NoError(t, f.db.Model(&row).Update("execution_descriptor", mustJSON(t, descriptor)).Error)
			if moved {
				require.NoError(t, f.db.Model(&models.Job{}).Where("id = ?", f.jobID).Update("namespace", "finance").Error)
			}

			result, err := New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{BaselineRunID: f.runID})
			require.NoError(t, err)

			var replayed models.JobRun
			require.NoError(t, f.db.First(&replayed, "id = ?", result.Run.ID).Error)
			var params map[string]string
			require.NoError(t, json.Unmarshal(replayed.Params, &params))
			var replayRow models.TaskRun
			require.NoError(t, f.db.First(&replayRow, "job_run_id = ?", result.Run.ID).Error)
			var replayDescriptor models.TaskExecutionDescriptor
			require.NoError(t, json.Unmarshal(replayRow.ExecutionDescriptor, &replayDescriptor))
			require.Equal(t, f.runID, replayDescriptor.Baseline.BaselineRunID, "the descriptor keeps its baseline provenance")
			require.Equal(t, descriptor.Baseline.Namespace, replayDescriptor.Baseline.Namespace)

			if !moved {
				require.Equal(t, baselineFreshnessParams, params)
				require.Equal(t, baselineFreshnessParams, replayDescriptor.Run.Params)
				return
			}
			want := map[string]string{"mode": "baseline", "_trigger_depth": "1"}
			require.Equal(t, want, params)
			require.Equal(t, want, replayDescriptor.Run.Params)
		})
	}
}
