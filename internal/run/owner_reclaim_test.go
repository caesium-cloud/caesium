package run

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// expireClaim rewrites a task's durable claim to look like the worker that held
// it died: the lease lapsed and a container id was left behind.
func expireClaim(t *testing.T, db *gorm.DB, runID, taskID uuid.UUID) {
	t.Helper()
	require.NoError(t, db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		Updates(map[string]interface{}{
			"claim_expires_at": time.Now().UTC().Add(-time.Minute),
			"runtime_id":       "container-abc",
		}).Error)
}

func taskRunFor(t *testing.T, db *gorm.DB, runID, taskID uuid.UUID) models.TaskRun {
	t.Helper()
	var row models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&row).Error)
	return row
}

// TestOwnerManager_ReclaimsExpiredWorkerClaimAndCompletesRun pins owner-side
// claim reaping.
//
// A worker that dies mid-task leaves its row `running` with a lapsed
// claim_expires_at.  Claimer.ReclaimExpired refuses to touch it — its live-lease
// guard deliberately skips rows belonging to a run whose owner is alive — and the
// owner only ever re-queued in-flight work on TAKEOVER.  So for a healthy owner
// the instance stayed `running` in memory forever: it consumed a
// fanOut.maxParallel slot, was never re-dispatched, and the run never completed.
func TestOwnerManager_ReclaimsExpiredWorkerClaimAndCompletesRun(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, taskA, taskB := seedTwoTaskRun(t, db, store, "")

	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3}).
		WithReclaimInterval(time.Millisecond)
	require.NoError(t, mgr.Adopt(runID, 1))

	ready := mgr.ReadyForDispatch(runID)
	require.Len(t, ready, 1)
	require.Equal(t, taskA, ready[0].TaskID)

	// Dispatch a for real: claim the row, then record it in memory.  The lease
	// stamped in memory is the dispatch deadline, and the durable claim carries
	// the same one; the worker holding it then dies, so BOTH lapse — which is
	// what the elapsed timestamps below stand in for.
	_, err := store.ClaimTaskForDispatch(runID, taskA, "node-1", 1, 1, 30*time.Second, true)
	require.NoError(t, err)
	mgr.MarkDispatched(runID, taskA, "node-1", 1, time.Now().Add(-time.Minute).UnixMilli())
	require.Empty(t, mgr.ReadyForDispatch(runID), "a dispatched task must leave the ready queue")

	expireClaim(t, db, runID, taskA)
	require.Equal(t, string(TaskStatusRunning), taskRunFor(t, db, runID, taskA).Status)

	// Nothing recovers it today; with the reaper wired in, the next tick does.
	requeued := mgr.ReclaimExpiredClaims(runID)
	require.Equal(t, []uuid.UUID{taskA}, requeued, "the expired in-flight task must be re-queued")

	row := taskRunFor(t, db, runID, taskA)
	require.Equal(t, string(TaskStatusPending), row.Status)
	require.Equal(t, "", row.ClaimedBy, "the dead worker's claim must be cleared so the row can be re-claimed")
	require.Equal(t, "", row.RuntimeID)
	require.Nil(t, row.StartedAt)
	require.Nil(t, row.ClaimExpiresAt)

	ready = mgr.ReadyForDispatch(runID)
	require.Len(t, ready, 1)
	require.Equal(t, taskA, ready[0].TaskID)
	require.Equal(t, 2, ready[0].Attempt, "a re-dispatch after a lost worker is a new attempt")

	// Re-dispatch and drive the run to completion.
	_, err = store.ClaimTaskForDispatch(runID, taskA, "node-2", 1, 1, 30*time.Second, true)
	require.NoError(t, err)
	mgr.MarkDispatched(runID, taskA, "node-2", 2, time.Now().Add(30*time.Second).UnixMilli())
	res, err := mgr.Complete(runID, taskA, TaskStatusSucceeded, "success", "", "node-2", nil, nil)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{taskB}, res.Ready)

	_, err = store.ClaimTaskForDispatch(runID, taskB, "node-2", 1, 1, 30*time.Second, true)
	require.NoError(t, err)
	mgr.MarkDispatched(runID, taskB, "node-2", 1, time.Now().Add(30*time.Second).UnixMilli())
	res, err = mgr.Complete(runID, taskB, TaskStatusSucceeded, "success", "", "node-2", nil, nil)
	require.NoError(t, err)
	require.True(t, res.Complete, "the run must finish once the lost task is re-run")

	var jobRun models.JobRun
	require.NoError(t, db.First(&jobRun, "id = ?", runID).Error)
	require.Equal(t, string(StatusSucceeded), jobRun.Status)
}

func TestRecoverPreservesExecutionAttemptSeparateFromClaimAttempt(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db).WithLeaseStore(NewLeaseStore(db))
	runID, taskA, _ := seedTwoTaskRun(t, db, store, "")
	claimExpiry := time.Now().UTC().Add(time.Minute)
	require.NoError(t, db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", runID, taskA).
		Updates(map[string]any{
			"status":           string(TaskStatusRunning),
			"claimed_by":       "worker-a",
			"claim_expires_at": claimExpiry,
			"attempt":          2,
			"claim_attempt":    7,
			"owner_generation": int64(1),
			"runtime_id":       "container-a",
		}).Error)

	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	_, err := mgr.RecoverVersion(runID, LeaseVersion{Generation: 1, StateRevision: 1})
	require.NoError(t, err)
	or, ok := mgr.get(runID)
	require.True(t, ok)
	state, ok := or.state.TaskState(taskA)
	require.True(t, ok)
	require.Equal(t, 2, state.Attempt,
		"RunState carries execution retry Attempt, never the claim-attempt ABA counter")
	require.Equal(t, "worker-a", state.ClaimedBy)
	require.True(t, state.Started)

	row := taskRunFor(t, db, runID, taskA)
	require.Equal(t, 2, row.Attempt)
	require.Equal(t, 7, row.ClaimAttempt)
}

// TestOwnerManager_ReclaimLeavesLiveClaimsAlone: a claim that has NOT lapsed is
// live work.  Resetting it would race the worker into double-execution — the
// exact thing the claimer's live-lease guard exists to prevent.
func TestOwnerManager_ReclaimLeavesLiveClaimsAlone(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, taskA, _ := seedTwoTaskRun(t, db, store, "")

	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3}).
		WithReclaimInterval(time.Millisecond)
	require.NoError(t, mgr.Adopt(runID, 1))
	_, err := store.ClaimTaskForDispatch(runID, taskA, "node-1", 1, 1, time.Hour, true)
	require.NoError(t, err)
	mgr.MarkDispatched(runID, taskA, "node-1", 1, time.Now().Add(-time.Minute).UnixMilli())

	// The owner's own lease bookkeeping is stale (the worker renewed the durable
	// claim without telling it), so the reap RUNS — and must still find nothing.
	require.Empty(t, mgr.ReclaimExpiredClaims(runID),
		"the durable claim_expires_at is authoritative, not the owner's stale copy")
	require.Equal(t, string(TaskStatusRunning), taskRunFor(t, db, runID, taskA).Status)
	require.Empty(t, mgr.ReadyForDispatch(runID))
}

// TestReclaimOwnerExpiredClaims_FencesOnNewerOwnerGeneration: a node that lost
// the lease must not reset claims out from under the node that now holds it.
func TestReclaimOwnerExpiredClaims_FencesOnNewerOwnerGeneration(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, taskA, _ := seedTwoTaskRun(t, db, store, "")

	_, err := store.ClaimTaskForDispatch(runID, taskA, "node-2", 5, 1, 30*time.Second, true)
	require.NoError(t, err)
	expireClaim(t, db, runID, taskA)

	stale, err := store.ReclaimOwnerExpiredClaims(runID, 1)
	require.NoError(t, err)
	require.Empty(t, stale, "an owner at generation 1 must not reset a row stamped generation 5")
	require.Equal(t, string(TaskStatusRunning), taskRunFor(t, db, runID, taskA).Status)

	current, err := store.ReclaimOwnerExpiredClaims(runID, 5)
	require.NoError(t, err)
	require.Len(t, current, 1, "the current owner reclaims its own expired claim")
	require.Equal(t, string(TaskStatusPending), taskRunFor(t, db, runID, taskA).Status)
}

// TestOwnerManager_ReclaimIsANoOpForUnownedOrIdleRuns keeps the reaper off the
// hot path: it must not query at all for a run this node does not own, nor for
// one with nothing in flight.
func TestOwnerManager_ReclaimIsANoOpForUnownedOrIdleRuns(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, _, _ := seedTwoTaskRun(t, db, store, "")

	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3}).
		WithReclaimInterval(time.Millisecond)
	require.Nil(t, mgr.ReclaimExpiredClaims(uuid.New()), "an unowned run is not this node's to reap")

	require.NoError(t, mgr.Adopt(runID, 1))
	require.Nil(t, mgr.ReclaimExpiredClaims(runID), "a run with nothing in flight has nothing to reap")
}

// TestRunState_RequeueExpiredRowsMatchesInstanceIdentity: a fan-out instance is
// keyed in this state by its TaskRun id, an unfanned step by its catalog task
// id.  The reaper resolves rows the same way SyncStartedFromRows does, or it
// silently re-queues nothing for exactly the fanned groups that need it most.
func TestRunState_RequeueExpiredRowsMatchesInstanceIdentity(t *testing.T) {
	producer, shard := uuid.New(), uuid.New()
	instance := uuid.New()
	topo := RunTopology{
		Adjacency:    map[uuid.UUID][]uuid.UUID{producer: {shard}},
		Predecessors: map[uuid.UUID][]uuid.UUID{shard: {producer}},
		Order:        map[uuid.UUID]int{producer: 0, shard: 1},
	}
	rs := NewRunState(topo, 0)
	rs.ExpandTask(shard, []ExpandedInstance{{TaskRunID: instance, TaskID: shard, PartitionIndex: 0, OutstandingPredecessors: 0}})
	rs.MarkDispatched(instance, "node-1", 1, time.Now().Add(-time.Minute).UnixMilli())
	rs.MarkDispatched(producer, "node-1", 1, time.Now().Add(-time.Minute).UnixMilli())

	requeued := rs.RequeueExpiredRows([]models.TaskRun{
		{ID: instance, TaskID: shard},
		{ID: uuid.New(), TaskID: producer}, // unfanned: the row id is not a node
	})
	require.ElementsMatch(t, []uuid.UUID{instance, producer}, requeued)

	instState, ok := rs.TaskState(instance)
	require.True(t, ok)
	require.Equal(t, TaskStatusPending, instState.Status)
	require.Equal(t, 2, instState.Attempt)
	require.Equal(t, "", instState.ClaimedBy)
	require.False(t, instState.Started)

	require.ElementsMatch(t, []uuid.UUID{producer, instance}, rs.ReadyTasks())

	// A row for a task that is no longer running is ignored.
	rs.ApplyCompletion(producer, TaskStatusSucceeded, nil)
	require.Empty(t, rs.RequeueExpiredRows([]models.TaskRun{{ID: uuid.New(), TaskID: producer}}))
}
