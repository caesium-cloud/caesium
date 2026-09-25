package dispatch

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
			require.Equal(t, ReasonTerminalRun, refusal.Code)
			server := httptest.NewServer(http.HandlerFunc(h.HandleComplete))
			_, postErr := PostComplete(t.Context(), server.URL, testToken, req)
			server.Close()
			require.ErrorIs(t, postErr, ErrOwnerRejected,
				"a terminal duplicate must be a worker ownership fence")
			require.NotErrorIs(t, postErr, ErrCompletionApplicationRejected)
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

// A replacement can commit cancellation after HandleComplete's lease read but
// before the owner's guarded task write. Keep the lease in this fixture to
// model that already-validated read, then drive the real HTTP handler and
// owner-manager path against terminal rows. A wrong claim on a running run
// remains distinguishable from this durable terminal fence.
func TestHandleCompleteMemoryOwnerTerminalClaimMismatch(t *testing.T) {
	store, ls, h := setupHandler(t)
	runID, taskID := seedPendingTaskRun(t, store)
	_, err := ls.AcquireLease(t.Context(), runID, ownerNodeAddr, time.Hour)
	require.NoError(t, err)
	require.NoError(t, store.ClaimTaskForDispatch(runID, taskID, ownerNodeAddr, 1, time.Hour, true))
	manager := run.NewOwnerManager(store, run.CheckpointConfig{})
	_, err = manager.Recover(runID, 1)
	require.NoError(t, err)
	require.NoError(t, store.ClaimTaskForDispatch(runID, taskID, ownerNodeAddr, 1, time.Hour, true))
	var claimed models.TaskRun
	require.NoError(t, store.DB().Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&claimed).Error)
	manager.MarkDispatched(runID, taskID, ownerNodeAddr, 1, time.Now().Add(time.Hour).UnixMilli())
	h.WithOwnerManager(manager)
	req := CompleteRequest{
		RunID: runID, TaskID: taskID, TaskRunID: claimed.ID,
		OwnerGeneration: 1, WorkerNode: "wrong-worker",
		Status: "succeeded", Result: "stale-result",
		Outputs: map[string]string{"stale_output": "must-not-persist"},
	}
	wrong := postJSON(t, h.HandleComplete, req)
	require.Equal(t, http.StatusConflict, wrong.Code, wrong.Body.String())
	var refusal ErrorResponse
	require.NoError(t, json.Unmarshal(wrong.Body.Bytes(), &refusal))
	require.Equal(t, ReasonWrongWorker, refusal.Code)
	var stillRunning models.TaskRun
	require.NoError(t, store.DB().First(&stillRunning, "id = ?", claimed.ID).Error)
	require.Equal(t, string(run.TaskStatusRunning), stillRunning.Status)
	require.Equal(t, ownerNodeAddr, stillRunning.ClaimedBy)
	require.Empty(t, stillRunning.Result)
	require.Empty(t, stillRunning.Output)
	require.Zero(t, stillRunning.TerminalSequence)

	require.NoError(t, store.DB().Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.JobRun{}).Where("id = ?", runID).
			Update("status", string(run.StatusCancelled)).Error; err != nil {
			return err
		}
		return tx.Model(&models.TaskRun{}).Where("id = ?", claimed.ID).
			Updates(map[string]any{"status": string(run.TaskStatusCancelled), "claimed_by": ""}).Error
	}))
	var beforeTask models.TaskRun
	require.NoError(t, store.DB().First(&beforeTask, "id = ?", claimed.ID).Error)
	var beforeEvents int64
	require.NoError(t, store.DB().Model(&models.ExecutionEvent{}).Where("run_id = ?", runID).Count(&beforeEvents).Error)
	req.WorkerNode = ownerNodeAddr
	terminal := postJSON(t, h.HandleComplete, req)
	require.Equal(t, http.StatusConflict, terminal.Code, terminal.Body.String())
	require.NoError(t, json.Unmarshal(terminal.Body.Bytes(), &refusal))
	require.Equal(t, ReasonTerminalRun, refusal.Code)
	var afterTask models.TaskRun
	require.NoError(t, store.DB().First(&afterTask, "id = ?", claimed.ID).Error)
	require.Equal(t, beforeTask.Status, afterTask.Status)
	require.Equal(t, beforeTask.ClaimedBy, afterTask.ClaimedBy)
	require.Equal(t, beforeTask.Result, afterTask.Result)
	require.Equal(t, beforeTask.Output, afterTask.Output)
	require.Equal(t, beforeTask.TerminalSequence, afterTask.TerminalSequence)
	var afterEvents int64
	require.NoError(t, store.DB().Model(&models.ExecutionEvent{}).Where("run_id = ?", runID).Count(&afterEvents).Error)
	require.Equal(t, beforeEvents, afterEvents)
}

func TestHandleCompleteCancelledRunWithoutLeaseReturnsTerminalRun(t *testing.T) {
	store, ls, h := setupHandler(t)
	runID, taskID := seedPendingTaskRun(t, store)
	_, err := ls.AcquireLease(t.Context(), runID, ownerNodeAddr, time.Hour)
	require.NoError(t, err)
	require.NoError(t, store.ClaimTaskForDispatch(runID, taskID, ownerNodeAddr, 1, time.Hour, true))
	var claimed models.TaskRun
	require.NoError(t, store.DB().Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&claimed).Error)
	h.WithOwnerManager(run.NewOwnerManager(store, run.CheckpointConfig{}))
	req := CompleteRequest{
		RunID: runID, TaskID: taskID, TaskRunID: claimed.ID,
		OwnerGeneration: 1, WorkerNode: ownerNodeAddr,
		Status: "succeeded", Result: "stale-result",
		Outputs: map[string]string{"stale_output": "must-not-persist"},
	}

	// CancelRun commits the terminal status and lease deletion together. The
	// completion arrives only after that transaction, as in the race's
	// cancellation-first order.
	require.NoError(t, store.CancelRun(t.Context(), runID))
	_, err = ls.GetLease(t.Context(), runID)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	var beforeRun models.JobRun
	require.NoError(t, store.DB().First(&beforeRun, "id = ?", runID).Error)
	require.Equal(t, string(run.StatusCancelled), beforeRun.Status)
	var beforeTask models.TaskRun
	require.NoError(t, store.DB().First(&beforeTask, "id = ?", claimed.ID).Error)
	require.Equal(t, string(run.TaskStatusCancelled), beforeTask.Status)
	var beforeEvents int64
	require.NoError(t, store.DB().Model(&models.ExecutionEvent{}).Where("run_id = ?", runID).Count(&beforeEvents).Error)

	w := postJSON(t, h.HandleComplete, req)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	var response ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Equal(t, ReasonTerminalRun, response.Code)
	var afterRun models.JobRun
	require.NoError(t, store.DB().First(&afterRun, "id = ?", runID).Error)
	require.Equal(t, beforeRun, afterRun, "completion must not rewrite the cancelled run")
	var afterTask models.TaskRun
	require.NoError(t, store.DB().First(&afterTask, "id = ?", claimed.ID).Error)
	require.Equal(t, beforeTask, afterTask, "completion must not persist task effects")
	var afterEvents int64
	require.NoError(t, store.DB().Model(&models.ExecutionEvent{}).Where("run_id = ?", runID).Count(&afterEvents).Error)
	require.Equal(t, beforeEvents, afterEvents)
}

func TestHandleCompleteMissingLeaseClassifiesDurableStatus(t *testing.T) {
	store, _, h := setupHandler(t)
	runID, taskID := seedPendingTaskRun(t, store)
	req := CompleteRequest{
		RunID: runID, TaskID: taskID, OwnerGeneration: 1,
		WorkerNode: ownerNodeAddr, Status: "succeeded",
	}

	running := postJSON(t, h.HandleComplete, req)
	require.Equal(t, http.StatusConflict, running.Code, running.Body.String())
	var response ErrorResponse
	require.NoError(t, json.Unmarshal(running.Body.Bytes(), &response))
	require.Equal(t, ReasonMissingRun, response.Code)

	const callback = "test:missing_lease_status_read_error"
	injectedReads := 0
	require.NoError(t, store.DB().Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "job_runs" {
			injectedReads++
			tx.AddError(errors.New("injected job-run read failure"))
		}
	}))
	unreadable := postJSON(t, h.HandleComplete, req)
	require.NoError(t, store.DB().Callback().Query().Remove(callback))
	require.Equal(t, 1, injectedReads, "the missing-lease status read must be exercised")
	require.Equal(t, http.StatusServiceUnavailable, unreadable.Code, unreadable.Body.String())
	require.NoError(t, json.Unmarshal(unreadable.Body.Bytes(), &response))
	require.Equal(t, ReasonOwnerNotReady, response.Code)

	// LeaseStore returns nil, nil when it is unavailable; that is an
	// unreadable fence, never proof that the lease or run is absent.
	h.leaseStore = nil
	noLeaseStore := postJSON(t, h.HandleComplete, req)
	require.Equal(t, http.StatusServiceUnavailable, noLeaseStore.Code, noLeaseStore.Body.String())
	require.NoError(t, json.Unmarshal(noLeaseStore.Body.Bytes(), &response))
	require.Equal(t, ReasonOwnerNotReady, response.Code)
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
	require.NoError(t, store.DB().Model(&models.JobRun{}).Where("id = ?", runID).
		Update("status", string(run.StatusSucceeded)).Error)
	req := CompleteRequest{
		RunID: runID, TaskID: taskID, TaskRunID: claimed.ID,
		OwnerGeneration: 1, WorkerNode: ownerNodeAddr,
		Status: "succeeded", Result: "success",
	}
	baseline := postJSON(t, h.HandleComplete, req)
	require.Equal(t, http.StatusConflict, baseline.Code, "terminal baseline: %s", baseline.Body.String())
	var baselineResponse ErrorResponse
	require.NoError(t, json.Unmarshal(baseline.Body.Bytes(), &baselineResponse))
	require.Equal(t, ReasonTerminalRun, baselineResponse.Code)
	const callback = "test:terminal_status_read_error"
	injectedReads := 0
	require.NoError(t, store.DB().Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "job_runs" {
			injectedReads++
			tx.AddError(errors.New("injected job-run read failure"))
		}
	}))
	w := postJSON(t, h.HandleComplete, req)
	require.NoError(t, store.DB().Callback().Query().Remove(callback))
	require.Equal(t, 1, injectedReads, "the injected status read must be exercised")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, "unreadable status must not be a terminal refusal: %s", w.Body.String())
	var response ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Equal(t, ReasonOwnerNotReady, response.Code)
	var row models.TaskRun
	require.NoError(t, store.DB().First(&row, "id = ?", claimed.ID).Error)
	require.Equal(t, string(run.TaskStatusRunning), row.Status)
	again := postJSON(t, h.HandleComplete, req)
	require.Equal(t, http.StatusConflict, again.Code, "status read recovered: %s", again.Body.String())
	var againResponse ErrorResponse
	require.NoError(t, json.Unmarshal(again.Body.Bytes(), &againResponse))
	require.Equal(t, ReasonTerminalRun, againResponse.Code)
}

func TestHandleCompleteLeaseReadErrorRetries(t *testing.T) {
	store, ls, h := setupHandler(t)
	runID, taskID := seedPendingTaskRun(t, store)
	_, err := ls.AcquireLease(t.Context(), runID, ownerNodeAddr, time.Hour)
	require.NoError(t, err)
	require.NoError(t, store.ClaimTaskForDispatch(runID, taskID, ownerNodeAddr, 1, time.Hour, true))
	var claimed models.TaskRun
	require.NoError(t, store.DB().Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&claimed).Error)
	req := CompleteRequest{
		RunID: runID, TaskID: taskID, TaskRunID: claimed.ID,
		OwnerGeneration: 1, WorkerNode: ownerNodeAddr,
		Status: "succeeded", Result: "completed-result",
	}
	const callback = "test:lease_read_error"
	injectedReads := 0
	require.NoError(t, store.DB().Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "run_leases" {
			injectedReads++
			tx.AddError(errors.New("injected lease read failure"))
		}
	}))
	defer func() { require.NoError(t, store.DB().Callback().Query().Remove(callback)) }()
	w := postJSON(t, h.HandleComplete, req)
	require.Greater(t, injectedReads, 0, "the injected lease read must be exercised")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	var response ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Equal(t, ReasonOwnerNotReady, response.Code)
	server := httptest.NewServer(http.HandlerFunc(h.HandleComplete))
	_, postErr := PostComplete(t.Context(), server.URL, testToken, req)
	server.Close()
	require.ErrorIs(t, postErr, ErrOwnerNotReady, "worker must retain a completed result after an unreadable lease")
	var after models.TaskRun
	require.NoError(t, store.DB().First(&after, "id = ?", claimed.ID).Error)
	require.Equal(t, string(run.TaskStatusRunning), after.Status)
	require.Equal(t, claimed.ClaimedBy, after.ClaimedBy)
	require.Empty(t, after.Result)
	require.Zero(t, after.TerminalSequence)
}
