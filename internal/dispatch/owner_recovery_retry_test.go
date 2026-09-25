package dispatch

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
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

func TestHandleCompleteMemoryOwnerTerminalDuplicate(t *testing.T) {
	for _, tc := range []struct {
		name, result, wantRun string
	}{
		{"succeeded", "success", string(run.StatusSucceeded)},
		{"failed", "failure", string(run.StatusFailed)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, ls, h := setupHandler(t)
			runID, taskID := seedPendingTaskRun(t, store)
			_, err := ls.AcquireLease(t.Context(), runID, ownerNodeAddr, time.Hour)
			require.NoError(t, err)
			require.NoError(t, store.ClaimTaskForDispatch(runID, taskID, ownerNodeAddr, 1, time.Hour, true))
			var claimed models.TaskRun
			require.NoError(t, store.DB().Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&claimed).Error)
			manager := run.NewOwnerManager(store, run.CheckpointConfig{})
			_, err = manager.Recover(runID, 1)
			require.NoError(t, err)
			require.NoError(t, store.ClaimTaskForDispatch(runID, taskID, ownerNodeAddr, 1, time.Hour, true))
			require.NoError(t, store.DB().Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&claimed).Error)
			manager.MarkDispatched(runID, taskID, ownerNodeAddr, 1, time.Now().Add(time.Hour).UnixMilli())
			h.WithOwnerManager(manager)
			req := CompleteRequest{
				RunID: runID, TaskID: taskID, TaskRunID: claimed.ID,
				OwnerGeneration: 1, WorkerNode: ownerNodeAddr,
				Status: tc.name, Result: tc.result, Error: "fixture failure",
			}
			first := postJSON(t, h.HandleComplete, req)
			require.Equal(t, http.StatusOK, first.Code, "first completion: %s", first.Body.String())
			var accepted CompleteResponse
			require.NoError(t, json.Unmarshal(first.Body.Bytes(), &accepted))
			require.True(t, accepted.Accepted)
			var beforeRun models.JobRun
			require.NoError(t, store.DB().First(&beforeRun, "id = ?", runID).Error)
			require.Equal(t, tc.wantRun, beforeRun.Status)
			var beforeTask models.TaskRun
			require.NoError(t, store.DB().First(&beforeTask, "id = ?", claimed.ID).Error)
			var beforeEvents int64
			require.NoError(t, store.DB().Model(&models.ExecutionEvent{}).Where("run_id = ?", runID).Count(&beforeEvents).Error)

			duplicate := postJSON(t, h.HandleComplete, req)
			require.Equal(t, http.StatusConflict, duplicate.Code, "terminal duplicate: %s", duplicate.Body.String())
			var refusal ErrorResponse
			require.NoError(t, json.Unmarshal(duplicate.Body.Bytes(), &refusal))
			require.Equal(t, ReasonTaskNotRunning, refusal.Code)
			var afterRun models.JobRun
			require.NoError(t, store.DB().First(&afterRun, "id = ?", runID).Error)
			require.Equal(t, beforeRun.Status, afterRun.Status)
			var afterTask models.TaskRun
			require.NoError(t, store.DB().First(&afterTask, "id = ?", claimed.ID).Error)
			require.Equal(t, beforeTask.Status, afterTask.Status)
			require.Equal(t, beforeTask.ClaimedBy, afterTask.ClaimedBy)
			require.Equal(t, beforeTask.TerminalSequence, afterTask.TerminalSequence)
			var afterEvents int64
			require.NoError(t, store.DB().Model(&models.ExecutionEvent{}).Where("run_id = ?", runID).Count(&afterEvents).Error)
			require.Equal(t, beforeEvents, afterEvents, "terminal duplicate must not append an event")

			if tc.name == "failed" {
				_, err := store.RetryFromFailure(runID)
				require.NoError(t, err)
				var reopened models.JobRun
				require.NoError(t, store.DB().First(&reopened, "id = ?", runID).Error)
				require.Equal(t, string(run.StatusRunning), reopened.Status)
				var beforeReopenedTask models.TaskRun
				require.NoError(t, store.DB().First(&beforeReopenedTask, "id = ?", claimed.ID).Error)
				untracked := postJSON(t, h.HandleComplete, req)
				require.Equal(t, http.StatusServiceUnavailable, untracked.Code,
					"reopened run must stay retryable while its owner rebuilds: %s", untracked.Body.String())
				var retryable ErrorResponse
				require.NoError(t, json.Unmarshal(untracked.Body.Bytes(), &retryable))
				require.Equal(t, ReasonOwnerNotReady, retryable.Code)
				var afterReopenedTask models.TaskRun
				require.NoError(t, store.DB().First(&afterReopenedTask, "id = ?", claimed.ID).Error)
				require.Equal(t, beforeReopenedTask.Status, afterReopenedTask.Status)
				require.Equal(t, beforeReopenedTask.ClaimedBy, afterReopenedTask.ClaimedBy)
			}
		})
	}
}

func TestHandleCompleteMemoryOwnerStatusReadErrorRetries(t *testing.T) {
	store, ls, h := setupHandler(t)
	runID, taskID := seedPendingTaskRun(t, store)
	_, err := ls.AcquireLease(t.Context(), runID, ownerNodeAddr, time.Hour)
	require.NoError(t, err)
	require.NoError(t, store.ClaimTaskForDispatch(runID, taskID, ownerNodeAddr, 1, time.Hour, true))
	var claimed models.TaskRun
	require.NoError(t, store.DB().Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&claimed).Error)
	h.WithOwnerManager(run.NewOwnerManager(store, run.CheckpointConfig{}))
	const callback = "test:terminal_status_read_error"
	require.NoError(t, store.DB().Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "job_runs" {
			tx.AddError(errors.New("injected job-run read failure"))
		}
	}))
	w := postJSON(t, h.HandleComplete, CompleteRequest{
		RunID: runID, TaskID: taskID, TaskRunID: claimed.ID,
		OwnerGeneration: 1, WorkerNode: ownerNodeAddr,
		Status: "succeeded", Result: "success",
	})
	require.NoError(t, store.DB().Callback().Query().Remove(callback))
	require.Equal(t, http.StatusServiceUnavailable, w.Code, "unreadable status must not be a terminal refusal: %s", w.Body.String())
	var response ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Equal(t, ReasonOwnerNotReady, response.Code)
	var row models.TaskRun
	require.NoError(t, store.DB().First(&row, "id = ?", claimed.ID).Error)
	require.Equal(t, string(run.TaskStatusRunning), row.Status)
}
