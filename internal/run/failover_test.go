package run

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestFailover_TakeoverAndResume exercises the full owner-crash failover path
// deterministically (no dqlite cluster, so no quorum-disruption confound):
//
//	owner A runs root a to completion and dispatches successor b (in-flight) →
//	A "dies" (its lease is expired) → owner B's sweep AcquireExpiredLeases takes
//	the lease (generation bumped) → B.Recover replays from A's checkpoint +
//	terminal rows and re-queues b → B re-claims and completes b → run finalized.
func TestFailover_TakeoverAndResume(t *testing.T) {
	ctx := context.Background()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := NewLeaseStore(db)
	store := NewStore(db).WithLeaseStore(ls)

	runID, a, b := seedTwoTaskRun(t, db, store, "node-A")
	// Both rows start claimed by node-A (HandleDispatch would set this on dispatch).
	require.NoError(t, db.Model(&models.TaskRun{}).Where("job_run_id = ?", runID).Update("claimed_by", "node-A").Error)

	cfg := CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3}

	// --- Owner A ---
	_, err := ls.AcquireLease(ctx, runID, "node-A", 10*time.Second)
	require.NoError(t, err)
	mgrA := NewOwnerManager(store, cfg)
	require.NoError(t, mgrA.Adopt(runID, 1))

	mgrA.MarkDispatched(runID, a, "node-A", 1, 0)
	resA, err := mgrA.Complete(runID, a, TaskStatusSucceeded, "success", "", "node-A", nil, nil)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{b}, resA.Ready, "completing a readies b (and writes a checkpoint at Events=1)")

	// b is dispatched and in-flight on A when A crashes: mark its DB row running.
	mgrA.MarkDispatched(runID, b, "node-A", 1, 0)
	require.NoError(t, db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", runID, b).
		Updates(map[string]interface{}{"status": string(TaskStatusRunning), "claimed_by": "node-A"}).Error)

	// --- Owner A dies: expire its lease ---
	require.NoError(t, db.Model(&models.RunLease{}).
		Where("run_id = ?", runID.String()).
		Update("lease_expires_at", time.Now().UTC().Add(-time.Second)).Error)

	// --- Owner B's failover sweep takes over the expired lease ---
	n, err := ls.AcquireExpiredLeases(ctx, "node-B", 10*time.Second)
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "the expired lease must be taken over")
	owned, err := ls.OwnedRunsWithGenerations(ctx, "node-B")
	require.NoError(t, err)
	genB, ok := owned[runID]
	require.True(t, ok, "node-B now owns the run")
	require.Equal(t, int64(2), genB, "takeover bumps the generation")

	// --- Owner B recovers and resumes ---
	mgrB := NewOwnerManager(store, cfg)
	res, err := mgrB.Recover(runID, genB)
	require.NoError(t, err)
	require.False(t, res.Complete, "run is not complete — b still needs to run")
	// b is the work to resume (ready from the checkpoint, with its DB row reset
	// to claimable by ResetInFlightTasks).
	require.Contains(t, append(res.Ready, res.ReDispatch...), b, "b must be queued for re-dispatch")

	// Re-claim b as node-B (HandleDispatch's claim) and complete it.
	require.NoError(t, store.ClaimTaskForDispatch(runID, b, "node-B", genB, time.Minute, true))
	mgrB.MarkDispatched(runID, b, "node-B", 1, 0)
	resB, err := mgrB.Complete(runID, b, TaskStatusSucceeded, "success", "", "node-B", nil, nil)
	require.NoError(t, err)
	require.True(t, resB.Complete, "run completes after b finishes on the new owner")

	// The run is finalized as succeeded.
	var jr models.JobRun
	require.NoError(t, db.First(&jr, "id = ?", runID).Error)
	require.Equal(t, string(StatusSucceeded), jr.Status, "run must be finalized succeeded after failover")
}
