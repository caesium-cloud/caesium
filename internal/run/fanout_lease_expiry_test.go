package run

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// addFanInSuccessorWithGate hangs a cross-step successor off the fanned group so
// a test can observe the group actually fanning in — the group's outcome is only
// visible from outside it through the successor it releases — AND gives that
// successor a second, independent predecessor (the "gate").
//
// The gate is what makes "the group fans in exactly ONCE" assertable rather than
// merely asserted. batchDecrementPredecessorsTx floor-clamps
// (`CASE WHEN outstanding_predecessors > 0 THEN outstanding_predecessors - 1 ELSE
// 0 END`, internal/run/store.go), so against a single-predecessor successor N
// decrements and one decrement land on the same value and are indistinguishable
// at the destination. With two predecessors there is headroom: a surplus group
// decrement drives the successor to zero and releases it EARLY, while the gate is
// still running — which is what the post-group assertions detect.
func addFanInSuccessorWithGate(t *testing.T, f *fanOutFixture, name string) (successor, gate *models.Task) {
	t.Helper()

	var consumerRun models.TaskRun
	require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ?", f.runID, f.consumer.ID).
		First(&consumerRun).Error)

	now := time.Now().UTC()
	mkTask := func(taskName string, position int) *models.Task {
		task := &models.Task{
			ID: uuid.New(), JobID: f.jobID, AtomID: consumerRun.AtomID, Name: taskName,
			Position: position, Type: "task", TriggerRule: jobdefschema.TriggerRuleAllSuccess,
		}
		require.NoError(t, f.db.Create(task).Error)
		return task
	}
	mkRun := func(task *models.Task, outstanding int) {
		require.NoError(t, f.db.Create(&models.TaskRun{
			ID: uuid.New(), JobRunID: f.runID, TaskID: task.ID, AtomID: consumerRun.AtomID,
			Engine: consumerRun.Engine, Image: consumerRun.Image, Command: consumerRun.Command,
			Status: string(TaskStatusPending), Attempt: 1, MaxAttempts: 1,
			OutstandingPredecessors: outstanding, CreatedAt: now, UpdatedAt: now,
		}).Error)
	}

	// gate is an unfanned root that runs alongside the group.
	gate = mkTask(name+"-gate", 2)
	successor = mkTask(name, 3)
	for _, e := range [][2]uuid.UUID{
		{f.consumer.ID, successor.ID}, {gate.ID, successor.ID},
	} {
		require.NoError(t, f.db.Create(&models.TaskEdge{
			ID: uuid.New(), JobID: f.jobID, FromTaskID: e[0], ToTaskID: e[1],
		}).Error)
	}
	mkRun(gate, 0)
	mkRun(successor, 2)
	return successor, gate
}

// killWorkerHoldingInstanceClaim rewrites one instance's durable claim to look
// like the worker holding it died mid-partition: the claim lease lapsed and a
// container id was left behind.  The row stays `running`, which is exactly the
// state the distributed lane leaves behind and deliberately does NOT write off
// (follow-up 4: the local sweep resolves such a row `skipped` because local mode
// has no recovery owner; the distributed lane leaves it for reclaim instead).
func killWorkerHoldingInstanceClaim(t *testing.T, f *fanOutFixture, taskRunID uuid.UUID) {
	t.Helper()
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("id = ?", taskRunID).
		Updates(map[string]interface{}{
			"claim_expires_at": time.Now().UTC().Add(-time.Minute),
			"runtime_id":       "container-partition-c",
		}).Error)
}

func instanceRowByKey(t *testing.T, f *fanOutFixture, key string) models.TaskRun {
	t.Helper()
	var row models.TaskRun
	require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ? AND partition_value = ?",
		f.runID, f.consumer.ID, key).First(&row).Error)
	return row
}

// TestFanOutLeaseExpiryMidGroupReDispatchesOnlyThatInstance closes follow-up 4's
// untested half: the DISTRIBUTED lane's answer to a lost instance outcome.
//
// The two lanes resolve it differently on purpose.  Local mode's post-group
// sweep writes the row off `skipped` because there is no recovery owner to
// revisit it.  The distributed lane leaves the row alone: the claim lease
// expires, the owner's reclaim returns it to the dispatchable pool, and the work
// is retried rather than written off.  Only the local half had coverage.
//
// This drives the distributed half through the SQL advancement path's real store
// seams — including the same reclaim the dispatch loop calls each tick before
// polling (DispatchLoop.reclaimExpiredClaims / dispatchRunInMemory) — and pins
// the four properties that matter mid-group: only the dead instance comes back,
// the siblings that already finished are untouched, the freed
// fanOut.maxParallel slot is returned so the group cannot wedge behind it, and
// the group still fans in exactly once (see addFanInSuccessorWithGate for what
// makes that last one assertable rather than merely asserted).
//
// It is an in-package store-level test, not a `test/` scenario: a claim only
// lapses when a worker dies holding it, and the integration lanes have no way to
// kill a worker mid-partition.
func TestFanOutLeaseExpiryMidGroupReDispatchesOnlyThatInstance(t *testing.T) {
	ctx := context.Background()
	f := newFanOutFixture(t, &jobdefschema.FanOut{
		From: "discover", MaxPartitions: 16, MaxParallel: 2,
	})
	publish, gate := addFanInSuccessorWithGate(t, f, "publish")

	leases := NewLeaseStore(f.db)
	f.store = f.store.WithLeaseStore(leases)
	generation, err := leases.AcquireLease(ctx, f.runID, "node-1", 30*time.Second)
	require.NoError(t, err)
	require.Equal(t, int64(1), generation)

	// Put the gate in flight up front so it never appears in the poll results
	// below; it is completed last, after the group has resolved.
	_, err = f.store.ClaimTaskForDispatch(f.runID, gate.ID, "worker-gate", generation, 1, time.Minute, false)
	require.NoError(t, err)

	// The producer runs and completes through the claimed (distributed) path,
	// expanding the group inside its own completion transaction.
	_, err = f.store.ClaimTaskForDispatch(f.runID, f.producer.ID, "worker-1", generation, 1, time.Minute, false)
	require.NoError(t, err)
	require.NoError(t, f.store.CompleteTaskClaimedWithPartitions(
		f.runID, f.producer.ID, "success", "worker-1", nil, nil, strParts("a", "b", "c")))

	pending, err := f.store.PendingTasksForDispatch(ctx, f.runID, 10)
	require.NoError(t, err)
	require.Len(t, pending, 3, "the whole group is dispatchable; the fan-in successor still waits")

	a := instanceRowByKey(t, f, "a")
	b := instanceRowByKey(t, f, "b")
	c := instanceRowByKey(t, f, "c")

	// maxParallel is enforced in the claim itself, so only two partitions run.
	_, err = f.store.ClaimTaskForDispatch(f.runID, a.ID, "worker-1", generation, 1, time.Minute, false)
	require.NoError(t, err)
	_, err = f.store.ClaimTaskForDispatch(f.runID, b.ID, "worker-1", generation, 1, time.Minute, false)
	require.NoError(t, err)
	_, err = f.store.ClaimTaskForDispatch(f.runID, c.ID, "worker-1", generation, 1, time.Minute, false)
	require.ErrorIs(t, err,
		ErrTaskClaimMismatch, "fanOut.maxParallel must refuse a third concurrent instance")

	// Partition a finishes, freeing a slot for c on a second worker.
	require.NoError(t, f.store.CompleteTaskClaimedWithPartitions(
		f.runID, a.ID, "success", "worker-1", nil, nil, nil))
	aAfterSuccess := instanceRowByKey(t, f, "a")
	require.Equal(t, string(TaskStatusSucceeded), aAfterSuccess.Status)
	require.NotNil(t, aAfterSuccess.CompletedAt)

	_, err = f.store.ClaimTaskForDispatch(f.runID, c.ID, "worker-2", generation, 1, time.Minute, false)
	require.NoError(t, err)

	// ...and worker-2 dies mid-partition.
	killWorkerHoldingInstanceClaim(t, f, c.ID)

	// The wedge this reclaim exists to prevent: c is `running` with a dead claim,
	// so the poll cannot see it (it only returns pending rows) and it still
	// occupies one of the group's two maxParallel slots forever.
	pending, err = f.store.PendingTasksForDispatch(ctx, f.runID, 10)
	require.NoError(t, err)
	require.Empty(t, pending, "precondition: the stuck instance is invisible to the dispatch poll")
	require.Equal(t, string(TaskStatusRunning), instanceRowByKey(t, f, "c").Status)

	// The owner's reclaim — the same call the dispatch loop makes before polling.
	reset, err := f.store.ReclaimOwnerExpiredClaims(f.runID, generation)
	require.NoError(t, err)
	require.Len(t, reset, 1, "only the instance whose claim lapsed may be reclaimed")
	require.Equal(t, c.ID, reset[0].ID)

	reclaimedC := instanceRowByKey(t, f, "c")
	require.Equal(t, string(TaskStatusPending), reclaimedC.Status)
	require.Equal(t, "", reclaimedC.ClaimedBy, "the dead worker's claim must be cleared for the re-dispatch")
	require.Equal(t, "", reclaimedC.RuntimeID)
	require.Nil(t, reclaimedC.ClaimExpiresAt)
	require.Nil(t, reclaimedC.CompletedAt,
		"the distributed lane retries the lost instance; it must NOT be written off as terminal "+
			"the way local mode's post-group sweep does")

	// The siblings are untouched: the finished one keeps its outcome and the
	// moment it landed, and the one still legitimately running keeps its live
	// claim (the reclaim's predicate is claim_expires_at, not group membership).
	finishedA := instanceRowByKey(t, f, "a")
	require.Equal(t, string(TaskStatusSucceeded), finishedA.Status)
	require.Equal(t, aAfterSuccess.CompletedAt, finishedA.CompletedAt)
	require.Equal(t, 1, finishedA.ClaimAttempt, "a finished sibling must not be re-claimed")

	stillRunningB := instanceRowByKey(t, f, "b")
	require.Equal(t, string(TaskStatusRunning), stillRunningB.Status)
	require.Equal(t, "worker-1", stillRunningB.ClaimedBy, "a live claim must not be reclaimed")
	require.Equal(t, 1, stillRunningB.ClaimAttempt)

	// Recovery hands exactly the lost instance back to the poll, and the freed
	// slot is available again.
	pending, err = f.store.PendingTasksForDispatch(ctx, f.runID, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, c.ID, pending[0].ID, "only the reclaimed instance is re-dispatched")

	_, err = f.store.ClaimTaskForDispatch(f.runID, c.ID, "worker-3", generation, 1, time.Minute, false)
	require.NoError(t, err)
	require.Equal(t, 2, instanceRowByKey(t, f, "c").ClaimAttempt,
		"the reclaimed instance is claimed a second time — this is a re-dispatch, not a resume")

	// The group finishes and fans in.
	require.NoError(t, f.store.CompleteTaskClaimedWithPartitions(
		f.runID, c.ID, "success", "worker-3", nil, nil, nil))
	require.NoError(t, f.store.CompleteTaskClaimedWithPartitions(
		f.runID, b.ID, "success", "worker-1", nil, nil, nil))

	// Every partition lands exactly once.  terminal_sequence is deliberately not
	// asserted here: it is the run-owner replay cursor, stamped by
	// CompleteTaskOwner, and SQL-mode advancement runs off
	// outstanding_predecessors instead.  The owner lane's sequences are pinned by
	// TestFailoverMidGroup_* in failover_fanout_test.go.
	rows := f.instances(t)
	require.Len(t, rows, 3, "the reclaim must not add an instance to the group")
	for _, row := range rows {
		require.Equalf(t, string(TaskStatusSucceeded), row.Status, "partition %s", row.PartitionValue)
		require.NotNilf(t, row.CompletedAt, "partition %s", row.PartitionValue)
	}

	// Exactly ONE group decrement reached the successor. This is the assertion
	// the gate exists for: publish started at two predecessors, the group is now
	// fully terminal, and the gate is still running — so a group that fanned in
	// twice would read 0 here and be dispatchable below, while one that never
	// fanned in would read 2.
	var publishRow models.TaskRun
	require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ?", f.runID, publish.ID).
		First(&publishRow).Error)
	require.Equal(t, 1, publishRow.OutstandingPredecessors,
		"the group must fan in exactly once — a lease-expired instance that was retried "+
			"resolves the group a single time, not once per completion")

	pending, err = f.store.PendingTasksForDispatch(ctx, f.runID, 10)
	require.NoError(t, err)
	require.Empty(t, pending, "the successor still waits on its non-group predecessor")

	// The gate lands and releases the successor — the group's decrement was real,
	// not merely absent.
	require.NoError(t, f.store.CompleteTaskClaimedWithPartitions(
		f.runID, gate.ID, "success", "worker-gate", nil, nil, nil))

	require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ?", f.runID, publish.ID).
		First(&publishRow).Error)
	require.Equal(t, 0, publishRow.OutstandingPredecessors)
	require.Equal(t, string(TaskStatusPending), publishRow.Status)

	pending, err = f.store.PendingTasksForDispatch(ctx, f.runID, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, publishRow.ID, pending[0].ID, "the fan-in successor is released exactly once")
}
