package run

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// TestOwnerManager_FenceRefusalLeavesNoCheckpointBehind pins the owner lane's
// half of the completion fence. A per-partition retry commits while the
// in-memory owner is completing the run: RetryPartition drops the run's
// checkpoints so a recovering owner replays the rows from scratch and finds
// the reset instance. If the owner then answers Complete's refusal
// (ErrRunHasPendingWork) by dropping the run WITH a forced checkpoint of its
// stale all-terminal state, recovery restores that snapshot, replays only the
// terminal rows after it, never sees the pending retry, and the run stays
// running forever. The owner must release the run without a checkpoint and
// discard any it wrote since the retry, leaving recovery the post-retry truth.
func TestOwnerManager_FenceRefusalLeavesNoCheckpointBehind(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, ids := seedChainRun(t, db, store, "node-1")
	taskA, taskB, taskC := ids[0], ids[1], ids[2]

	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	require.NoError(t, mgr.Adopt(runID, 1))
	mgr.MarkDispatched(runID, taskA, "node-1", 1, 0)
	_, err := mgr.Complete(runID, taskA, TaskStatusSucceeded, "success", "", "node-1", nil, nil)
	require.NoError(t, err)
	mgr.MarkDispatched(runID, taskB, "node-1", 1, 0)
	_, err = mgr.Complete(runID, taskB, TaskStatusSucceeded, "success", "", "node-1", nil, nil)
	require.NoError(t, err)

	// A retry-reset instance lands while the owner believes the run is about
	// to complete: a task the adopted state never tracked, pending and marked.
	// RetryPartition would have dropped the checkpoints at this point.
	var jobRun models.JobRun
	require.NoError(t, db.First(&jobRun, "id = ?", runID).Error)
	var atomModel models.Atom
	require.NoError(t, db.First(&atomModel).Error)
	now := time.Now().UTC()
	retried := &models.Task{ID: uuid.New(), JobID: jobRun.JobID, AtomID: atomModel.ID, Name: "retried", Position: 3, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(retried).Error)
	retriedInstance := uuid.New()
	require.NoError(t, db.Create(&models.TaskRun{
		ID: retriedInstance, JobRunID: runID, TaskID: retried.ID, AtomID: atomModel.ID,
		Engine: atomModel.Engine, Image: atomModel.Image, Command: atomModel.Command,
		Status: string(TaskStatusPending), Attempt: 1, MaxAttempts: 1,
		PartitionValue: "p", PartitionCount: 1, PartitionRetryPending: true,
		CreatedAt: now, UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Where("run_id = ?", runID.String()).Delete(&models.RunCheckpoint{}).Error)

	mgr.MarkDispatched(runID, taskC, "node-1", 1, 0)
	res, err := mgr.Complete(runID, taskC, TaskStatusSucceeded, "success", "", "node-1", nil, nil)
	require.NoError(t, err)
	require.True(t, res.Complete, "precondition: the owner's state believes the run is complete")

	got, err := store.Get(runID)
	require.NoError(t, err)
	require.Equal(t, StatusRunning, got.Status, "the fence must keep the run open for the pending retry")
	require.False(t, mgr.Owns(runID), "the owner has no engine for the retry and must release the run")

	cp, err := store.LatestFullCheckpoint(runID)
	require.NoError(t, err)
	require.Nil(t, cp, "no checkpoint may survive the refusal: every snapshot the owner holds predates the retry")

	// A recovering owner must rebuild from the rows and find the retry ready.
	takeover := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	rec, err := takeover.Recover(runID, 2)
	require.NoError(t, err)
	readyIDs := map[uuid.UUID]bool{}
	for _, id := range rec.Ready {
		readyIDs[id] = true
	}
	require.True(t, readyIDs[retriedInstance] || readyIDs[retried.ID],
		"recovery from scratch must surface the pending retry as dispatchable (ready=%v)", rec.Ready)
}

// TestOwnerManager_ReleasedRunNeverCheckpointsAgain pins the race behind the
// release: a worker completion that captured the owned-run pointer before
// Release forgot it can still reach its checkpoint write afterwards. Every
// snapshot that state can produce predates the retry, so once the run is
// released as stale no writer may checkpoint it — not the cadence write in
// Complete, not the forced write in Drop.
func TestOwnerManager_ReleasedRunNeverCheckpointsAgain(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, ids := seedChainRun(t, db, store, "node-1")

	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	require.NoError(t, mgr.Adopt(runID, 1))
	mgr.MarkDispatched(runID, ids[0], "node-1", 1, 0)
	_, err := mgr.Complete(runID, ids[0], TaskStatusSucceeded, "success", "", "node-1", nil, nil)
	require.NoError(t, err)

	// A completion in flight holds the pointer across the release.
	mgr.mu.Lock()
	or := mgr.runs[runID]
	mgr.mu.Unlock()
	require.NotNil(t, or)

	mgr.Release(runID)
	require.NoError(t, store.InvalidateRunCheckpoints(runID))
	cp, err := store.LatestFullCheckpoint(runID)
	require.NoError(t, err)
	require.Nil(t, cp, "precondition: nothing survives the release")

	// The late writer paths the in-flight completion (and a racing Drop) use.
	or.mu.Lock()
	or.checkpointMaybe()
	or.checkpointForce()
	or.mu.Unlock()

	cp, err = store.LatestFullCheckpoint(runID)
	require.NoError(t, err)
	require.Nil(t, cp, "a released run's stale state must never be checkpointed again")
}

// TestOwnerManager_ReleaseInvalidatesCheckpointsWhileStillOwned pins the
// ordering of the release. Once the owner is forgotten, a recovery tick may
// adopt the run at any moment; if the stale checkpoints were still on disk at
// that point, recovery would restore the pre-retry state into a fresh owner
// that the later invalidation cannot reach. So the checkpoints must be gone —
// and the run already marked stale, so nothing writes a new one — BEFORE the
// owner is forgotten.
func TestOwnerManager_ReleaseInvalidatesCheckpointsWhileStillOwned(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, ids := seedChainRun(t, db, store, "node-1")

	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	require.NoError(t, mgr.Adopt(runID, 1))
	mgr.MarkDispatched(runID, ids[0], "node-1", 1, 0)
	_, err := mgr.Complete(runID, ids[0], TaskStatusSucceeded, "success", "", "node-1", nil, nil)
	require.NoError(t, err)
	cp, err := store.LatestFullCheckpoint(runID)
	require.NoError(t, err)
	require.NotNil(t, cp, "precondition: a checkpoint exists to invalidate")

	mgr.mu.Lock()
	or := mgr.runs[runID]
	mgr.mu.Unlock()
	require.NotNil(t, or)

	// Observe the run's state at the moment its checkpoints are deleted.
	var deletes atomic.Int32
	var ownedAtDelete, staleAtDelete atomic.Bool
	const name = "test:observe_checkpoint_delete"
	require.NoError(t, db.Callback().Delete().Before("gorm:delete").Register(name, func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != "run_checkpoints" {
			return
		}
		deletes.Add(1)
		ownedAtDelete.Store(mgr.Owns(runID))
		or.mu.Lock()
		staleAtDelete.Store(or.stale)
		or.mu.Unlock()
	}))
	t.Cleanup(func() { _ = db.Callback().Delete().Remove(name) })

	require.NoError(t, mgr.Release(runID))

	require.GreaterOrEqual(t, deletes.Load(), int32(1), "release must invalidate the run's checkpoints")
	require.True(t, ownedAtDelete.Load(), "checkpoints must be invalidated before the owner is forgotten")
	require.True(t, staleAtDelete.Load(), "the run must already be stale when its checkpoints are invalidated, so none is rewritten")
	require.False(t, mgr.Owns(runID))
	cp, err = store.LatestFullCheckpoint(runID)
	require.NoError(t, err)
	require.Nil(t, cp)
}

// TestOwnerManager_ReleaseKeepsOwnershipWhenInvalidationFails pins the
// failure edge of the release: if the checkpoints cannot be deleted, the
// stale snapshot is still on disk, and forgetting the owner would let a
// recovery tick restore it. The owner must keep the run (stale, so it never
// checkpoints again) and report the error instead.
func TestOwnerManager_ReleaseKeepsOwnershipWhenInvalidationFails(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, ids := seedChainRun(t, db, store, "node-1")

	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	require.NoError(t, mgr.Adopt(runID, 1))
	mgr.MarkDispatched(runID, ids[0], "node-1", 1, 0)
	_, err := mgr.Complete(runID, ids[0], TaskStatusSucceeded, "success", "", "node-1", nil, nil)
	require.NoError(t, err)

	const name = "test:fail_checkpoint_delete"
	require.NoError(t, db.Callback().Delete().Before("gorm:delete").Register(name, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "run_checkpoints" {
			_ = tx.AddError(errors.New("simulated checkpoint delete failure"))
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Delete().Remove(name) })

	require.Error(t, mgr.Release(runID), "a release that could not discard the checkpoints must say so")
	require.True(t, mgr.Owns(runID), "the owner must keep the run while a stale checkpoint survives")
	cp, err := store.LatestFullCheckpoint(runID)
	require.NoError(t, err)
	require.NotNil(t, cp, "precondition: the stale checkpoint is still on disk")
}

// TestOwnerManager_StaleRunReleaseIsRetriedOnReclaimTick pins the recovery of
// a release that could not discard its checkpoints. The owner keeps the run
// (stale) so the surviving snapshot cannot be restored, but it must not keep
// it forever: the dispatch loop's per-run reclaim tick retries the release
// until the store cooperates, at which point the run is forgotten and a
// recovering owner can rebuild it from the rows.
func TestOwnerManager_StaleRunReleaseIsRetriedOnReclaimTick(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, ids := seedChainRun(t, db, store, "node-1")

	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	require.NoError(t, mgr.Adopt(runID, 1))
	mgr.MarkDispatched(runID, ids[0], "node-1", 1, 0)
	_, err := mgr.Complete(runID, ids[0], TaskStatusSucceeded, "success", "", "node-1", nil, nil)
	require.NoError(t, err)

	// The first checkpoint delete fails; later ones succeed.
	var failures atomic.Int32
	const name = "test:fail_checkpoint_delete_once"
	require.NoError(t, db.Callback().Delete().Before("gorm:delete").Register(name, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "run_checkpoints" && failures.Add(1) == 1 {
			_ = tx.AddError(errors.New("simulated checkpoint delete failure"))
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Delete().Remove(name) })

	require.Error(t, mgr.Release(runID))
	require.True(t, mgr.Owns(runID), "precondition: the owner kept the run")

	// The next reclaim tick retries the release.
	require.Nil(t, mgr.ReclaimExpiredClaims(runID))
	require.False(t, mgr.Owns(runID), "the reclaim tick must retry a stale run's release once the store cooperates")
	cp, err := store.LatestFullCheckpoint(runID)
	require.NoError(t, err)
	require.Nil(t, cp)
}
