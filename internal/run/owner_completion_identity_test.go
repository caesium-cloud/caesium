package run

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// The owner's in-memory RunState keys an UNFANNED task by its CATALOG task id.
// That is the invariant ReadyForDispatch encodes when it leaves
// DispatchableTask.TaskRunID at uuid.Nil for anything CatalogTaskID does not
// call an instance, and the invariant ExpandTask maintains when it swaps a
// template node for per-instance nodes.
//
// The worker's completion envelope does NOT follow that rule: ownerSink.send
// stamps req.TaskRunID = taskRun.ID unconditionally, for every route and every
// task (internal/worker/completion_sink.go). That is right for the SQL
// fallback, where loadTaskRunByIDOrUnique takes a primary key or a catalog id
// interchangeably — but the owner's map lookup accepts only the key it stored.
//
// So CompleteInstance's `if taskRunID != uuid.Nil { identity = taskRunID }`
// looked an unfanned task's TaskRun primary key up in a map keyed by its
// catalog id, missed, and ApplyCompletion returned Applied=false with
// TerminalSequence 0. Nothing was persisted (res.Durable() is false), the DAG
// never advanced, and the run hung until the harness timed out — the whole
// owner in-memory lane, not one scenario, because every job starts with an
// unfanned task.
//
// The existing owner tests all pass uuid.Nil for an unfanned completion, which
// is what let this ship green. These drive CompleteInstance the way the worker
// actually calls it.

type ownerIdentityFixture struct {
	db       *gorm.DB
	store    *Store
	mgr      *OwnerManager
	runID    uuid.UUID
	producer *models.Task
	process  *models.Task
	rowID    map[uuid.UUID]uuid.UUID // catalog task id -> TaskRun primary key
}

func newOwnerIdentityFixture(t *testing.T, fanned bool) *ownerIdentityFixture {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	now := time.Now().UTC()
	trigger := &models.Trigger{ID: uuid.New(), Alias: "oid-trig-" + uuid.NewString()[:8], Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(trigger).Error)
	job := &models.Job{ID: uuid.New(), Alias: "oid-job-" + uuid.NewString()[:8], TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(job).Error)

	runRecord, err := store.Start(job.ID, &trigger.ID)
	require.NoError(t, err)

	atomModel := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(atomModel).Error)

	producer := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atomModel.ID, Name: "list", Position: 0, CreatedAt: now, UpdatedAt: now}
	process := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atomModel.ID, Name: "process", Position: 1, CreatedAt: now, UpdatedAt: now}
	if fanned {
		process.FanOutConfig = datatypes.JSON([]byte(`{"from":"list","maxPartitions":8}`))
	}
	require.NoError(t, db.Create([]*models.Task{producer, process}).Error)
	require.NoError(t, db.Create(&models.TaskEdge{ID: uuid.New(), JobID: job.ID, FromTaskID: producer.ID, ToTaskID: process.ID, CreatedAt: now}).Error)

	rowID := map[uuid.UUID]uuid.UUID{}
	mk := func(task *models.Task, outstanding int) {
		row := &models.TaskRun{
			ID: uuid.New(), JobRunID: runRecord.ID, TaskID: task.ID, AtomID: atomModel.ID,
			Engine: atomModel.Engine, Image: atomModel.Image, Command: atomModel.Command,
			Status: string(TaskStatusPending), ClaimedBy: "node-1", Attempt: 1, MaxAttempts: 1,
			OutstandingPredecessors: outstanding, CreatedAt: now, UpdatedAt: now,
		}
		require.NoError(t, db.Create(row).Error)
		rowID[task.ID] = row.ID
	}
	mk(producer, 0)
	mk(process, 1)

	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	require.NoError(t, mgr.Adopt(runRecord.ID, 1))
	mgr.MarkDispatched(runRecord.ID, producer.ID, "node-1", 1, 0)

	return &ownerIdentityFixture{
		db: db, store: store, mgr: mgr, runID: runRecord.ID,
		producer: producer, process: process, rowID: rowID,
	}
}

func (f *ownerIdentityFixture) state(t *testing.T) *RunState {
	t.Helper()
	or, ok := f.mgr.get(f.runID)
	require.True(t, ok)
	return or.state
}

// TestOwnerCompleteInstanceAcceptsUnfannedTaskRunID is the stall: a fan-out
// producer is unfanned, so its completion arrives keyed by a TaskRun id the
// owner's state has never heard of.
func TestOwnerCompleteInstanceAcceptsUnfannedTaskRunID(t *testing.T) {
	f := newOwnerIdentityFixture(t, true)

	// Exactly what the worker sends: the catalog task id AND the executed row's
	// primary key, for a task that is not fanned.
	res, err := f.mgr.CompleteInstance(
		f.runID, f.producer.ID, f.rowID[f.producer.ID],
		TaskStatusSucceeded, "success", "", "node-1", nil, nil,
		[]pkgtask.Partition{{Key: "a"}, {Key: "b"}, {Key: "c"}},
	)
	require.NoError(t, err)

	require.Len(t, res.Ready, 3,
		"the producer's completion must advance the DAG and release the expanded group; "+
			"an unmatched identity drops the completion silently and the run hangs")
	require.Len(t, f.mgr.ReadyForDispatch(f.runID), 3,
		"the expansion must be applied to the owner's in-memory state")

	// The completion must also be DURABLE: a dropped completion stamps nothing,
	// so terminal_sequence stays 0 and the row is invisible to the replay tail a
	// recovering owner reads with `terminal_sequence > ?`.
	var producerRow models.TaskRun
	require.NoError(t, f.db.First(&producerRow, "id = ?", f.rowID[f.producer.ID]).Error)
	require.Equal(t, string(TaskStatusSucceeded), producerRow.Status)
	require.Greater(t, producerRow.TerminalSequence, int64(0),
		"a completion the owner applied must be persisted with its own terminal_sequence")

	var instances []models.TaskRun
	require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ?", f.runID, f.process.ID).Find(&instances).Error)
	require.Len(t, instances, 3, "the group must be materialized in the database, not only in memory")
}

// TestOwnerCompleteInstanceUnfannedRunReachesCompletion is the same defect at
// the other end of a run: a plain two-step DAG with no fan-out anywhere must
// still finish when every completion carries a TaskRunID. That is the shape of
// EVERY job in owner in-memory mode, fanned or not.
func TestOwnerCompleteInstanceUnfannedRunReachesCompletion(t *testing.T) {
	f := newOwnerIdentityFixture(t, false)

	res, err := f.mgr.CompleteInstance(
		f.runID, f.producer.ID, f.rowID[f.producer.ID],
		TaskStatusSucceeded, "success", "", "node-1", nil, nil, nil,
	)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{f.process.ID}, res.Ready,
		"the successor must become ready when its only predecessor completes")

	res, err = f.mgr.CompleteInstance(
		f.runID, f.process.ID, f.rowID[f.process.ID],
		TaskStatusSucceeded, "success", "", "node-1", nil, nil, nil,
	)
	require.NoError(t, err)
	require.True(t, res.Complete, "the run must reach completion rather than hang out its timeout")
}

// TestOwnerCompleteInstanceStillPrefersRealInstanceIdentity guards the fix from
// over-correcting: a FANNED instance is keyed in RunState by its TaskRun id and
// that identity must keep winning. Falling back to the catalog id for a fanned
// group would resolve one arbitrary sibling — the D5b defect in reverse.
func TestOwnerCompleteInstanceStillPrefersRealInstanceIdentity(t *testing.T) {
	f := newOwnerIdentityFixture(t, true)

	_, err := f.mgr.CompleteInstance(
		f.runID, f.producer.ID, f.rowID[f.producer.ID],
		TaskStatusSucceeded, "success", "", "node-1", nil, nil,
		[]pkgtask.Partition{{Key: "a"}, {Key: "b"}, {Key: "c"}},
	)
	require.NoError(t, err)

	ready := f.mgr.ReadyForDispatch(f.runID)
	require.Len(t, ready, 3)
	target := ready[0]
	require.NotEqual(t, uuid.Nil, target.TaskRunID, "a fanned instance is dispatched by row identity")
	require.Equal(t, f.process.ID, target.TaskID, "the catalog id is carried alongside, not instead")

	_, err = f.mgr.CompleteInstance(
		f.runID, target.TaskID, target.TaskRunID,
		TaskStatusSucceeded, "success", "", "node-1", nil, nil, nil,
	)
	require.NoError(t, err)

	st, ok := f.state(t).TaskState(target.TaskRunID)
	require.True(t, ok)
	require.Equal(t, TaskStatusSucceeded, st.Status, "the completion must resolve the instance it named")

	terminal := 0
	for _, dt := range ready {
		if s, ok := f.state(t).TaskState(dt.TaskRunID); ok && IsTerminal(s.Status) {
			terminal++
		}
	}
	require.Equal(t, 1, terminal, "only the named instance may be resolved")
}

// TestOwnerCompleteInstanceSuccessIsNotTurnedIntoAFailure pins the second half
// of the owner-lane stall, which the first fix uncovered.
//
// CompleteInstance plans a fan-out expansion on EVERY success, and it addressed
// the producer by catalog task id. For a fanned instance that id names N rows,
// so the single-row load returned ErrAmbiguousTaskRun — and the planning error
// branch rewrites `status` to failed. A partition that exited 0 was therefore
// recorded as failed, and under the default fail_fast policy that resolved its
// pending siblings and failed the run. The only trace was a `record not found`
// line in the query log.
func TestOwnerCompleteInstanceSuccessIsNotTurnedIntoAFailure(t *testing.T) {
	f := newOwnerIdentityFixture(t, true)

	_, err := f.mgr.CompleteInstance(
		f.runID, f.producer.ID, f.rowID[f.producer.ID],
		TaskStatusSucceeded, "success", "", "node-1", nil, nil,
		[]pkgtask.Partition{{Key: "a"}, {Key: "b"}, {Key: "c"}},
	)
	require.NoError(t, err)
	ready := f.mgr.ReadyForDispatch(f.runID)
	require.Len(t, ready, 3)

	res, err := f.mgr.CompleteInstance(
		f.runID, ready[0].TaskID, ready[0].TaskRunID,
		TaskStatusSucceeded, "success", "", "node-1", nil, nil, nil,
	)
	require.NoError(t, err)
	require.False(t, res.Complete,
		"two siblings are still pending; the group cannot have resolved")

	st, ok := f.state(t).TaskState(ready[0].TaskRunID)
	require.True(t, ok)
	require.Equal(t, TaskStatusSucceeded, st.Status,
		"a partition that exited 0 must not be recorded failed by a planning error")

	for _, dt := range ready[1:] {
		sib, ok := f.state(t).TaskState(dt.TaskRunID)
		require.True(t, ok)
		require.Equal(t, TaskStatusPending, sib.Status,
			"a sibling must not be cancelled by fail_fast when nothing actually failed")
	}

	var rows []models.TaskRun
	require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ?", f.runID, f.process.ID).Find(&rows).Error)
	for i := range rows {
		require.NotEqual(t, string(TaskStatusFailed), rows[i].Status,
			"partition %q must not be persisted failed", rows[i].PartitionValue)
	}
}

// TestOwnerCompleteInstanceDerivesFailureFromResult pins the owner lane's half
// of the same route-completeness defect the SQL lane had.
//
// A container that exits non-zero is NOT reported as a failure. The worker
// treats "the container ran and told us its result" as a completion and calls
// sink.Succeeded, so ownerSink posts Status="succeeded" with Result="failure"
// (internal/worker/completion_sink.go). The SQL lane re-derives the real status
// from the result string (taskStatusFromResult, inside completeTask); the owner
// took req.Status at face value, recorded the instance SUCCEEDED in memory, and
// so never ran the fail_fast branch — every pending sibling kept running.
//
// The durable row still went failed, but only via the LATER sink.Failed
// delivery, which lands after the DAG has already advanced: the row said failed
// while the owner's state said succeeded. One reported outcome must produce one
// status on both sides.
func TestOwnerCompleteInstanceDerivesFailureFromResult(t *testing.T) {
	f := newOwnerIdentityFixture(t, true)

	_, err := f.mgr.CompleteInstance(
		f.runID, f.producer.ID, f.rowID[f.producer.ID],
		TaskStatusSucceeded, "success", "", "node-1", nil, nil,
		[]pkgtask.Partition{{Key: "a"}, {Key: "b"}, {Key: "c"}},
	)
	require.NoError(t, err)
	ready := f.mgr.ReadyForDispatch(f.runID)
	require.Len(t, ready, 3)

	// Held across the call: resolving the group makes the run complete, and a
	// complete run is dropped from the manager's map.
	state := f.state(t)

	// Exactly the envelope a non-zero exit produces.
	res, err := f.mgr.CompleteInstance(
		f.runID, ready[0].TaskID, ready[0].TaskRunID,
		TaskStatusSucceeded, "failure", "", "node-1", nil, nil, nil,
	)
	require.NoError(t, err)
	require.True(t, res.Complete,
		"fail_fast resolves the whole group, so the run finishes on this transition")

	st, ok := state.TaskState(ready[0].TaskRunID)
	require.True(t, ok)
	require.Equal(t, TaskStatusFailed, st.Status,
		"a `failure` result must be recorded failed by the owner, as the SQL lane records it")

	for _, dt := range ready[1:] {
		sib, ok := state.TaskState(dt.TaskRunID)
		require.True(t, ok)
		require.Equal(t, TaskStatusSkipped, sib.Status,
			"fail_fast must cancel a pending sibling in owner mode too")
	}

	// …and durably, with the reason both lanes emit.
	var rows []models.TaskRun
	require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ?", f.runID, f.process.ID).
		Order("partition_index ASC").Find(&rows).Error)
	require.Len(t, rows, 3)
	for i := range rows {
		if rows[i].ID == ready[0].TaskRunID {
			require.Equal(t, string(TaskStatusFailed), rows[i].Status,
				"partition %q ran and exited non-zero", rows[i].PartitionValue)
			continue
		}
		require.Equal(t, string(TaskStatusSkipped), rows[i].Status,
			"partition %q must be durably cancelled", rows[i].PartitionValue)
		require.Equal(t, "fan-out group failed fast", rows[i].Error)
	}
}

// TestOwnerStateIsInvalidatedByRetryFromFailure pins the third stall: a RETRIED
// run must not be dispatched from the snapshot taken before it was re-opened.
//
// When a run completes the owner drops its state — but the dispatch loop
// recovers any run whose lease it still holds, so a completed run is put back
// into the map with a state that says `complete`. RetryFromFailure then resets
// the rows to pending, and because dispatchRunInMemory only rebuilds a run it
// does NOT already own, that stale state is never refreshed: ReadyForDispatch
// returns nothing forever. The pull-path claimer will not rescue it either — the
// run still holds a live lease, and liveLeaseGuardSQL defers to the owner.
func TestOwnerStateIsInvalidatedByRetryFromFailure(t *testing.T) {
	f := newOwnerIdentityFixture(t, false)

	// Drive the run to a terminal failure through the owner, exactly as a run
	// would reach one in production.
	_, err := f.mgr.CompleteInstance(
		f.runID, f.producer.ID, f.rowID[f.producer.ID],
		TaskStatusSucceeded, "success", "", "node-1", nil, nil, nil,
	)
	require.NoError(t, err)
	res, err := f.mgr.CompleteInstance(
		f.runID, f.process.ID, f.rowID[f.process.ID],
		TaskStatusFailed, "failure", "boom", "node-1", nil, nil, nil,
	)
	require.NoError(t, err)
	require.True(t, res.Complete)

	// The dispatch loop still holds the run's lease, so it recovers the finished
	// run and republishes a `complete` state into the manager.
	_, err = f.mgr.Recover(f.runID, 1)
	require.NoError(t, err)
	require.True(t, f.mgr.Owns(f.runID), "the loop's recover re-publishes the completed run")
	require.Empty(t, f.mgr.ReadyForDispatch(f.runID))

	reopened, err := f.store.RetryFromFailure(f.runID)
	require.NoError(t, err)
	require.Equal(t, StatusRunning, reopened.Status)

	require.False(t, f.mgr.Owns(f.runID),
		"a re-opened run must not keep the snapshot that called it complete; "+
			"the dispatch loop only rebuilds a run it does not own")

	// …and the rebuild sees the reset row, so the run is dispatchable again.
	_, err = f.mgr.Recover(f.runID, 1)
	require.NoError(t, err)
	ready := f.mgr.ReadyForDispatch(f.runID)
	require.Len(t, ready, 1, "the retried task must be dispatchable after the rebuild")
	require.Equal(t, f.process.ID, ready[0].TaskID)
}
