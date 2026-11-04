package run

import (
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// withInstantStoreBusyRetry keeps contention retries on the production budget
// length but without the real backoff sleeps.
func withInstantStoreBusyRetry(t *testing.T) {
	t.Helper()
	orig := storeBusyRetryBackoffs
	storeBusyRetryBackoffs = make([]time.Duration, len(orig))
	t.Cleanup(func() { storeBusyRetryBackoffs = orig })
}

// TestOwnerCompletionDoesNotPersistCheckpointContention is issue #599.
// CompleteInstance plans the fan-out on every success, outside the retried
// terminal write. A dqlite "checkpoint in progress" from that read was stored
// as the task's final error and failed the run. A transient checkpoint must be
// retried into the real success; a checkpoint that outlasts the budget, or an
// unrelated error, must not be recorded as success.
func TestOwnerCompletionDoesNotPersistCheckpointContention(t *testing.T) {
	withInstantStoreBusyRetry(t)

	t.Run("transient checkpoint is retried into success", func(t *testing.T) {
		db, mgr, runID, taskA := ownerContentionRun(t)
		faults := 0
		registerQueryFault(t, db, "owner-contention-transient", func() error {
			if faults == 0 {
				faults++
				return errors.New("checkpoint in progress")
			}
			return nil
		})

		res, err := mgr.Complete(runID, taskA, TaskStatusSucceeded, "success", "", "node-1", nil, nil)
		require.NoError(t, err)
		require.True(t, res.Owned)
		require.Equal(t, 1, faults, "the checkpoint blip must happen once and then be retried")

		row := taskRunRow(t, db, runID, taskA)
		require.Equal(t, string(TaskStatusSucceeded), row.Status)
		require.Empty(t, row.Error)
		require.Equal(t, "success", row.Result)

		var runRow models.JobRun
		require.NoError(t, db.First(&runRow, "id = ?", runID).Error)
		require.Equal(t, string(StatusRunning), runRow.Status)
	})

	t.Run("persistent checkpoint is not a success", func(t *testing.T) {
		db, mgr, runID, taskA := ownerContentionRun(t)
		injected := 0
		registerQueryFault(t, db, "owner-contention-persistent", func() error {
			injected++
			return errors.New("checkpoint in progress")
		})

		_, err := mgr.Complete(runID, taskA, TaskStatusSucceeded, "success", "", "node-1", nil, nil)
		require.Error(t, err)
		require.True(t, isStoreContentionErr(err))
		require.Equal(t, len(storeBusyRetryBackoffs)+1, injected, "contention retries are bounded")
		// The fault fails every query, including the assertions below.
		require.NoError(t, db.Callback().Query().Remove("owner-contention-persistent"))

		row := taskRunRow(t, db, runID, taskA)
		require.Equal(t, string(TaskStatusPending), row.Status)
		require.NotContains(t, row.Error, "checkpoint in progress")

		var runRow models.JobRun
		require.NoError(t, db.First(&runRow, "id = ?", runID).Error)
		require.Equal(t, string(StatusRunning), runRow.Status)
		require.NotContains(t, runRow.Error, "checkpoint in progress")
	})

	t.Run("unrelated error is not a success", func(t *testing.T) {
		db, mgr, runID, taskA := ownerContentionRun(t)
		faults := 0
		registerQueryFault(t, db, "owner-contention-unrelated", func() error {
			if faults == 0 {
				faults++
				return errors.New("schema mismatch")
			}
			return nil
		})

		res, err := mgr.Complete(runID, taskA, TaskStatusSucceeded, "success", "", "node-1", nil, nil)
		require.NoError(t, err)
		require.True(t, res.Owned)
		require.Equal(t, 1, faults, "a non-contention error must not be retried")

		row := taskRunRow(t, db, runID, taskA)
		require.NotEqual(t, string(TaskStatusSucceeded), row.Status)
		require.Equal(t, string(TaskStatusFailed), row.Status)
		require.Contains(t, row.Error, "schema mismatch")
		require.NotContains(t, row.Error, "checkpoint in progress")
	})
}

func ownerContentionRun(t *testing.T) (*gorm.DB, *OwnerManager, uuid.UUID, uuid.UUID) {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, taskA, _ := seedTwoTaskRun(t, db, store, "node-1")
	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1000, Interval: time.Hour, KeepFulls: 3})
	require.NoError(t, mgr.Adopt(runID, 1))
	return db, mgr, runID, taskA
}

func taskRunRow(t *testing.T, db *gorm.DB, runID, taskID uuid.UUID) models.TaskRun {
	t.Helper()
	var row models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&row).Error)
	return row
}

func registerQueryFault(t *testing.T, db *gorm.DB, name string, fault func() error) {
	t.Helper()
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(name, func(tx *gorm.DB) {
		if err := fault(); err != nil {
			_ = tx.AddError(err)
		}
	}))
	t.Cleanup(func() {
		_ = db.Callback().Query().Remove(name)
	})
}
