package dispatch

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/stretchr/testify/require"
)

func TestHandleCompleteMemoryOwnerRetriesBeforeRecovery(t *testing.T) {
	for _, status := range []string{"succeeded", "cached", "failed"} {
		t.Run(status, func(t *testing.T) {
			store, ls, h := setupHandler(t)
			runID, taskID := seedPendingTaskRun(t, store)
			_, err := ls.AcquireLease(t.Context(), runID, ownerNodeAddr, time.Hour)
			require.NoError(t, err)
			require.NoError(t, store.ClaimTaskForDispatch(runID, taskID, ownerNodeAddr, 1, time.Hour, true))
			var claimed models.TaskRun
			require.NoError(t, store.DB().Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&claimed).Error)
			h.WithOwnerManager(run.NewOwnerManager(store, run.CheckpointConfig{}))
			w := postJSON(t, h.HandleComplete, CompleteRequest{
				RunID: runID, TaskID: taskID, TaskRunID: claimed.ID,
				OwnerGeneration: 1, WorkerNode: ownerNodeAddr,
				Status: status, Result: "success", Error: "failure",
			})
			require.Equal(t, http.StatusServiceUnavailable, w.Code)
			var response ErrorResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
			require.Equal(t, ReasonOwnerNotReady, response.Code)
			var row models.TaskRun
			require.NoError(t, store.DB().First(&row, "id = ?", claimed.ID).Error)
			require.Equal(t, string(run.TaskStatusRunning), row.Status, "retry must not fall through to the SQL terminal writer")
			require.Zero(t, row.TerminalSequence)
		})
	}
}
