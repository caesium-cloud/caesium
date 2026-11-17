package run

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// dispatchReady is the queue the dispatch loop posts from: a run it does not
// own is recovered first (dispatchRunInMemory), then ReadyForDispatch.
func dispatchReady(t *testing.T, mgr *OwnerManager, runID uuid.UUID) []DispatchableTask {
	t.Helper()
	if !mgr.Owns(runID) {
		_, err := mgr.Recover(runID, 1)
		require.NoError(t, err)
	}
	return mgr.ReadyForDispatch(runID)
}

func dispatchIDs(ready []DispatchableTask) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(ready))
	for _, dt := range ready {
		ids = append(ids, dt.ExecutionRef())
	}
	return ids
}

// TestAbandonPartitionRetriesDropsOwnerReadyQueue is the owner-memory
// partition-retry failure from #458. The replacement engine resolves a
// retry-reset instance it could not dispatch. That skip used to be a raw
// UPDATE (terminal_sequence left at 0) followed by invalidateRunState, which
// drops the live owner. The next dispatch tick recovers, TerminalTaskRunsSince
// does not replay a zero sequence, and the skipped instance is pending on the
// ready queue again — ClaimTaskForDispatch matches nothing and the loop
// re-posts it as task_not_running.
func TestAbandonPartitionRetriesDropsOwnerReadyQueue(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{
		From: "discover", MaxPartitions: 16, FailurePolicy: jobdefschema.FanOutFailureContinue,
	})
	_, err := f.expand(t, strParts("fail", "keep"))
	require.NoError(t, err)
	rows := finishFannedRun(t, f, TaskStatusFailed, TaskStatusSucceeded)
	failID := rows[0].ID
	keepID := rows[1].ID

	_, _, err = f.store.RetryPartition(context.Background(), f.runID, failID)
	require.NoError(t, err)

	// Producer and the succeeded sibling are replayable. The reset instance's
	// sequence is cleared so a skip that does not stamp one is invisible to
	// recovery — the shape that spun in CI.
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", f.producerRow(t).ID).
		Update("terminal_sequence", 1).Error)
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", keepID).
		Update("terminal_sequence", 2).Error)
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", failID).
		Updates(map[string]any{"terminal_sequence": 0}).Error)

	mgr := NewOwnerManager(f.store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	_, err = mgr.Recover(f.runID, 1)
	require.NoError(t, err)
	require.Contains(t, dispatchIDs(mgr.ReadyForDispatch(f.runID)), failID,
		"precondition: the retried instance is what the dispatch loop would post")

	reason := "partition retry abandoned: the replacement engine could not dispatch the reset instance"
	resolved, err := f.store.AbandonPartitionRetries(f.runID, []uuid.UUID{failID}, reason)
	require.NoError(t, err)
	require.Equal(t, 1, resolved)

	var skipped models.TaskRun
	require.NoError(t, f.db.First(&skipped, "id = ?", failID).Error)
	require.Equal(t, string(TaskStatusSkipped), skipped.Status)
	require.Equal(t, reason, skipped.Error)
	require.False(t, skipped.PartitionRetryPending)
	require.Greater(t, skipped.TerminalSequence, int64(0),
		"a takeover replays terminal_sequence > 0 only; 0 puts the row back on the ready queue")

	require.True(t, mgr.Owns(f.runID),
		"abandoning a retry folds the skip into the live owner; releasing it makes the next tick rebuild")
	require.NotContains(t, dispatchIDs(dispatchReady(t, mgr, f.runID)), failID)
	require.False(t, mgr.Dispatchable(f.runID, failID),
		"the dispatch loop re-checks Dispatchable immediately before POST")
	// A second tick must not rediscover the row. That rediscovery is the spin.
	require.NotContains(t, dispatchIDs(dispatchReady(t, mgr, f.runID)), failID)

	require.NoError(t, mgr.Release(f.runID))
	_, err = mgr.Recover(f.runID, 1)
	require.NoError(t, err)
	require.NotContains(t, dispatchIDs(mgr.ReadyForDispatch(f.runID)), failID,
		"a takeover must not re-queue an abandoned retry whose row is already skipped")
	require.False(t, mgr.Dispatchable(f.runID, failID))

	// The run can now finalize: the completion fence only refuses a still-pending retry.
	require.NoError(t, f.store.Complete(f.runID, errors.New(reason)))
	got, err := f.store.Get(f.runID)
	require.NoError(t, err)
	require.Equal(t, StatusFailed, got.Status)
}
