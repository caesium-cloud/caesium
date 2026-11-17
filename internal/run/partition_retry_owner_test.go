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

// TestAbandonPartitionRetriesSerializesWithOwnerCompletion is the window after
// the skip commits and before the owner fold. A completion that clones the
// pre-skip state there stamps the same terminal_sequence and checkpoints with
// the abandoned instance still pending. Recovery then filters the skip out
// (terminal_sequence is not greater than the checkpoint) and puts it back on
// the ready queue.
func TestAbandonPartitionRetriesSerializesWithOwnerCompletion(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{
		From: "discover", MaxPartitions: 16, FailurePolicy: jobdefschema.FanOutFailureContinue,
	})
	_, err := f.expand(t, strParts("abandon", "complete"))
	require.NoError(t, err)
	rows := finishFannedRun(t, f, TaskStatusFailed, TaskStatusFailed)
	abandonID := rows[0].ID
	completeID := rows[1].ID

	_, _, err = f.store.RetryPartition(context.Background(), f.runID, abandonID)
	require.NoError(t, err)
	_, _, err = f.store.RetryPartition(context.Background(), f.runID, completeID)
	require.NoError(t, err)

	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", f.producerRow(t).ID).
		Update("terminal_sequence", 1).Error)
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id IN ?", []uuid.UUID{abandonID, completeID}).
		Update("terminal_sequence", 0).Error)

	mgr := NewOwnerManager(f.store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	_, err = mgr.Recover(f.runID, 1)
	require.NoError(t, err)
	ready := dispatchIDs(mgr.ReadyForDispatch(f.runID))
	require.Contains(t, ready, abandonID)
	require.Contains(t, ready, completeID)

	finished := make(chan error, 1)
	f.store.afterAbandonRowsRead = func() {
		go func() {
			_, callErr := mgr.CompleteInstance(f.runID, f.consumer.ID, completeID, TaskStatusSucceeded, "success", "", "", nil, nil, nil)
			finished <- callErr
		}()
		select {
		case callErr := <-finished:
			t.Errorf("owner completion ran before abandonment folded the skip: %v", callErr)
			finished <- callErr
		case <-time.After(50 * time.Millisecond):
		}
	}

	reason := "partition retry abandoned: the replacement engine could not dispatch the reset instance"
	resolved, err := f.store.AbandonPartitionRetries(f.runID, []uuid.UUID{abandonID}, reason)
	require.NoError(t, err)
	require.Equal(t, 1, resolved)

	select {
	case callErr := <-finished:
		require.NoError(t, callErr)
	case <-time.After(5 * time.Second):
		t.Fatal("owner completion did not finish after abandonment released the lock")
	}

	var abandoned, completed models.TaskRun
	require.NoError(t, f.db.First(&abandoned, "id = ?", abandonID).Error)
	require.NoError(t, f.db.First(&completed, "id = ?", completeID).Error)
	require.Equal(t, string(TaskStatusSkipped), abandoned.Status)
	require.Equal(t, string(TaskStatusSucceeded), completed.Status)
	require.Greater(t, abandoned.TerminalSequence, int64(0))
	require.Greater(t, completed.TerminalSequence, int64(0))
	require.NotEqual(t, abandoned.TerminalSequence, completed.TerminalSequence,
		"the completion must not reuse the sequence the skip just allocated")

	require.NoError(t, mgr.Release(f.runID))
	_, err = mgr.Recover(f.runID, 1)
	require.NoError(t, err)
	recovered := dispatchIDs(mgr.ReadyForDispatch(f.runID))
	require.NotContains(t, recovered, abandonID,
		"recovery must not re-queue a skipped row whose sequence the checkpoint already covers")
	require.NotContains(t, recovered, completeID)
	require.False(t, mgr.Dispatchable(f.runID, abandonID))
}
