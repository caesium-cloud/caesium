package run

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestCancelledOwnerCompletionBookkeeping(t *testing.T) {
	for _, cause := range []error{NewRunCancellationError(context.Canceled),
		fmt.Errorf("owner stopped: %w", NewRunCancellationError(context.Canceled)),
		NewRunCancellationError(errors.New("custom shutdown cause"))} {
		t.Run(cause.Error(), func(t *testing.T) {
			t.Run("concrete instances, events, retry and idempotency", func(t *testing.T) {
				testRunTerminationBookkeeping(t, cause)
			})
			t.Run("rollback", func(t *testing.T) { testRunTerminationRollback(t, cause) })
			t.Run("task quarantine", func(t *testing.T) { testRunTerminationQuarantine(t, cause, false) })
			t.Run("run quarantine", func(t *testing.T) { testRunTerminationQuarantine(t, cause, true) })
		})
	}
}

func TestCancelledOwnerCompletionPreservesDurableCancellation(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "cancelled-owner-fence"}).Error)
	jr, err := store.Start(jobID, nil)
	require.NoError(t, err)
	row := models.TaskRun{ID: uuid.New(), JobRunID: jr.ID, TaskID: uuid.New(), Engine: models.AtomEngineDocker,
		Status: string(TaskStatusRunning), ClaimedBy: "worker", RuntimeID: "original-runtime"}
	require.NoError(t, db.Create(&row).Error)
	require.NoError(t, store.CancelRun(t.Context(), jr.ID))
	var beforeRun models.JobRun
	var beforeTask models.TaskRun
	require.NoError(t, db.First(&beforeRun, "id = ?", jr.ID).Error)
	require.NoError(t, db.First(&beforeTask, "id = ?", row.ID).Error)
	require.Equal(t, string(StatusCancelled), beforeRun.Status)
	require.Equal(t, "cancelled by concurrency replacement", beforeRun.Error)
	var beforeEvents []models.ExecutionEvent
	require.NoError(t, db.Where("run_id = ?", jr.ID).Order("sequence ASC").Find(&beforeEvents).Error)

	finalized, err := store.CompleteIfActive(jr.ID, NewRunCancellationError(fmt.Errorf("server stopped: %w", context.Canceled)))
	require.NoError(t, err)
	require.False(t, finalized)
	var afterRun models.JobRun
	var afterTask models.TaskRun
	var afterEvents []models.ExecutionEvent
	require.NoError(t, db.First(&afterRun, "id = ?", jr.ID).Error)
	require.NoError(t, db.First(&afterTask, "id = ?", row.ID).Error)
	require.NoError(t, db.Where("run_id = ?", jr.ID).Order("sequence ASC").Find(&afterEvents).Error)
	require.Equal(t, beforeRun, afterRun)
	require.Equal(t, beforeTask, afterTask)
	require.Equal(t, beforeEvents, afterEvents)
}

func TestOnlyWholeRunTerminationBypassesPendingRetryFence(t *testing.T) {
	for _, cause := range []error{nil, errors.New("ordinary task failure"), context.Canceled, fmt.Errorf("backend stopped: %w", context.Canceled), context.DeadlineExceeded,
		fmt.Errorf("task timeout: %w", context.DeadlineExceeded)} {
		name := "success"
		if cause != nil {
			name = cause.Error()
		}
		t.Run(name, func(t *testing.T) {
			db := testutil.OpenTestDB(t)
			t.Cleanup(func() { testutil.CloseDB(db) })
			store := NewStore(db)
			jobID := uuid.New()
			require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "non-cancel-fence"}).Error)
			jr, err := store.Start(jobID, nil)
			require.NoError(t, err)
			row := models.TaskRun{ID: uuid.New(), JobRunID: jr.ID, TaskID: uuid.New(), Status: string(TaskStatusPending), PartitionRetryPending: true}
			require.NoError(t, db.Create(&row).Error)
			finalized, err := store.CompleteIfActive(jr.ID, cause)
			require.ErrorIs(t, err, ErrRunHasPendingWork)
			require.False(t, finalized)
			var got models.TaskRun
			var runRow models.JobRun
			require.NoError(t, db.First(&got, "id = ?", row.ID).Error)
			require.NoError(t, db.First(&runRow, "id = ?", jr.ID).Error)
			require.Equal(t, string(StatusRunning), runRow.Status)
			require.Equal(t, string(TaskStatusPending), got.Status)
			require.True(t, got.PartitionRetryPending)
			require.Nil(t, got.CompletedAt)
			var count int64
			require.NoError(t, db.Model(&models.ExecutionEvent{}).Where("run_id = ? AND type IN ?", jr.ID,
				[]string{string(event.TypeTaskFailed), string(event.TypeRunTerminal)}).Count(&count).Error)
			require.Zero(t, count)
		})
	}
}

func TestCancelledCompletionFencesEachClaimAndUnrelatedRun(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	jobID, taskID := uuid.New(), uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "cancelled-claim-fence"}).Error)
	jr, err := store.Start(jobID, nil)
	require.NoError(t, err)
	other, err := store.Start(jobID, nil)
	require.NoError(t, err)
	lease := time.Now().UTC().Add(time.Minute)
	rows := []models.TaskRun{
		{ID: uuid.New(), JobRunID: jr.ID, TaskID: taskID, Status: string(TaskStatusRunning), ClaimedBy: "worker-a", ClaimExpiresAt: &lease, RuntimeID: "runtime-a", PartitionCount: 2, PartitionIndex: 0, PartitionValue: "a"},
		{ID: uuid.New(), JobRunID: jr.ID, TaskID: taskID, Status: string(TaskStatusRunning), ClaimedBy: "worker-b", ClaimExpiresAt: &lease, RuntimeID: "runtime-b", PartitionCount: 2, PartitionIndex: 1, PartitionValue: "b"},
		{ID: uuid.New(), JobRunID: other.ID, TaskID: taskID, Status: string(TaskStatusRunning), ClaimedBy: "worker-a", ClaimExpiresAt: &lease, RuntimeID: "unrelated"},
	}
	require.NoError(t, db.Create(&rows).Error)
	var untouched models.TaskRun
	require.NoError(t, db.First(&untouched, "id = ?", rows[2].ID).Error)
	require.NoError(t, store.Complete(jr.ID, NewRunCancellationError(context.Canceled)))
	for _, row := range rows[:2] {
		require.ErrorIs(t, store.EnsureTaskRunStartable(jr.ID, row.ID, row.ClaimedBy), ErrTaskClaimMismatch)
		require.ErrorIs(t, store.CompleteTaskClaimed(jr.ID, row.ID, "success", row.ClaimedBy, nil, nil), ErrTaskClaimMismatch)
		require.ErrorIs(t, store.FailTaskClaimed(jr.ID, row.ID, errors.New("late error"), row.ClaimedBy), ErrTaskClaimMismatch)
		var got models.TaskRun
		require.NoError(t, db.First(&got, "id = ?", row.ID).Error)
		require.Equal(t, string(TaskStatusFailed), got.Status)
		require.Equal(t, context.Canceled.Error(), got.Error)
		require.Equal(t, row.RuntimeID, got.RuntimeID)
		require.Empty(t, got.ClaimedBy)
		require.Nil(t, got.ClaimExpiresAt)
		require.NotNil(t, got.CompletedAt)
	}
	var stillUntouched models.TaskRun
	require.NoError(t, db.First(&stillUntouched, "id = ?", rows[2].ID).Error)
	require.Equal(t, untouched, stillUntouched)
	snapshot, err := store.Get(other.ID)
	require.NoError(t, err)
	require.Equal(t, StatusRunning, snapshot.Status)
}

func TestNonCancellationCompletionDoesNotSweepUnfinishedTasks(t *testing.T) {
	for _, cause := range []error{nil, errors.New("ordinary task failure"), context.Canceled, fmt.Errorf("backend stopped: %w", context.Canceled), context.DeadlineExceeded} {
		name := "success"
		if cause != nil {
			name = cause.Error()
		}
		t.Run(name, func(t *testing.T) {
			db := testutil.OpenTestDB(t)
			t.Cleanup(func() { testutil.CloseDB(db) })
			store := NewStore(db)
			jobID := uuid.New()
			require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "ordinary-completion"}).Error)
			jr, err := store.Start(jobID, nil)
			require.NoError(t, err)
			row := models.TaskRun{ID: uuid.New(), JobRunID: jr.ID, TaskID: uuid.New(), Status: string(TaskStatusRunning), ClaimedBy: "worker"}
			require.NoError(t, db.Create(&row).Error)
			var before models.TaskRun
			require.NoError(t, db.First(&before, "id = ?", row.ID).Error)
			require.NoError(t, store.Complete(jr.ID, cause))
			var after models.TaskRun
			require.NoError(t, db.First(&after, "id = ?", row.ID).Error)
			require.Equal(t, before, after)
		})
	}
}

func TestCancelledCompletionPreservesEveryTerminalTaskState(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "terminal-task-fence"}).Error)
	jr, err := store.Start(jobID, nil)
	require.NoError(t, err)
	completed := time.Now().UTC().Add(-time.Second)
	for i, status := range []TaskStatus{TaskStatusSucceeded, TaskStatusFailed, TaskStatusSkipped, TaskStatusCached, TaskStatusCancelled} {
		row := models.TaskRun{ID: uuid.New(), JobRunID: jr.ID, TaskID: uuid.New(), Status: string(status),
			CompletedAt: &completed, TerminalSequence: int64(i + 1), Error: "original cause", RuntimeID: "original-runtime", Output: []byte(`{"evidence":"keep"}`)}
		require.NoError(t, db.Create(&row).Error)
	}
	var before []models.TaskRun
	require.NoError(t, db.Where("job_run_id = ?", jr.ID).Order("id ASC").Find(&before).Error)
	require.NoError(t, store.Complete(jr.ID, NewRunCancellationError(context.Canceled)))
	var after []models.TaskRun
	require.NoError(t, db.Where("job_run_id = ?", jr.ID).Order("id ASC").Find(&after).Error)
	require.Equal(t, before, after)
	var count int64
	require.NoError(t, db.Model(&models.ExecutionEvent{}).Where("run_id = ? AND type = ?", jr.ID, string(event.TypeTaskFailed)).Count(&count).Error)
	require.Zero(t, count)
}

func TestRunCancellationMarkerPreservesAuthorityAndCause(t *testing.T) {
	custom := errors.New("authoritative server stopped")
	for _, cause := range []error{context.Canceled, fmt.Errorf("wrapped: %w", context.Canceled), custom} {
		marked := NewRunCancellationError(cause)
		require.Equal(t, cause.Error(), marked.Error())
		require.ErrorIs(t, marked, cause)
		require.True(t, IsRunCancellationError(fmt.Errorf("transport: %w", marked)))
		require.False(t, IsRunCancellationError(cause))
		require.False(t, IsRunDeadlineError(marked))
	}
	require.ErrorIs(t, NewRunCancellationError(nil), context.Canceled)
	require.False(t, IsRunCancellationError(context.DeadlineExceeded))
}
