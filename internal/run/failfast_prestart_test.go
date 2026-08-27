package run

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// The scenario every test in this file drives: a fail_fast group where one
// instance fails while a sibling has been CLAIMED but has not started. Both
// lanes flip a row to `running` at claim time, before any container exists, so
// "pending siblings" is not the set of siblings that can still be cancelled —
// and the sibling that slips through starts AFTER its group has already failed.
// It reproduced in CI only under scheduling pressure (one worker slot), which is
// why both lanes now cancel on "not started" rather than on "pending".

// --- owner in-memory lane -------------------------------------------------

// TestRunState_FailFastCancelsDispatchedButUnstartedSibling is the owner
// engine's half: a sibling the owner already DISPATCHED (a peer accepted the
// push; its worker pool has not created a container) must be cancelled, not
// left to start after the failure.
func TestRunState_FailFastCancelsDispatchedButUnstartedSibling(t *testing.T) {
	b := newTopoBuilder()
	producer := b.task("")
	fanned := b.task("")
	rs := NewRunState(b.build(), 0)

	rs.ApplyCompletion(producer, TaskStatusSucceeded, nil)
	insts := fannedGroup(t, rs, fanned, "process", 0, strParts("bad", "gate", "x"))
	bad, gate, x := insts[0], insts[1], insts[2]

	// One worker slot: both are dispatched, neither has started, and `x` is
	// still pending behind them.
	rs.MarkDispatched(bad, "node-1", 1, 0)
	rs.MarkDispatched(gate, "node-1", 1, 0)
	rs.MarkStarted(bad)

	res := rs.ApplyCompletion(bad, TaskStatusFailed, nil)

	skipped := map[uuid.UUID]string{}
	for _, sk := range res.Skipped {
		skipped[sk.TaskID] = sk.Reason
		require.Greater(t, sk.TerminalSequence, int64(0),
			"a cancelled sibling needs its own sequence or replay cannot see it")
	}
	require.Contains(t, skipped, gate,
		"a dispatched-but-unstarted sibling must be cancelled: fail_fast means no further instance STARTS")
	require.Equal(t, "fan-out group failed fast", skipped[gate])
	require.Contains(t, skipped, x, "a pending sibling is cancelled as before")

	st, ok := rs.TaskState(gate)
	require.True(t, ok)
	require.Equal(t, TaskStatusSkipped, st.Status)
	require.NotContains(t, rs.ReadyTasks(), gate, "a cancelled sibling must never become dispatchable again")
	require.True(t, rs.IsComplete(), "the whole group is resolved, so the run is complete")
}

// TestRunState_FailFastLeavesStartedSiblingRunning is the other side of the
// same predicate: a sibling whose container is up is left alone, because
// Caesium cannot kill it and a terminal row would be contradicted by its
// worker's later completion.
func TestRunState_FailFastLeavesStartedSiblingRunning(t *testing.T) {
	b := newTopoBuilder()
	producer := b.task("")
	fanned := b.task("")
	rs := NewRunState(b.build(), 0)

	rs.ApplyCompletion(producer, TaskStatusSucceeded, nil)
	insts := fannedGroup(t, rs, fanned, "process", 0, strParts("bad", "gate", "x"))
	bad, gate, x := insts[0], insts[1], insts[2]

	rs.MarkDispatched(bad, "node-1", 1, 0)
	rs.MarkDispatched(gate, "node-1", 1, 0)
	rs.MarkStarted(bad)
	rs.MarkStarted(gate)

	res := rs.ApplyCompletion(bad, TaskStatusFailed, nil)

	for _, sk := range res.Skipped {
		require.NotEqual(t, gate, sk.TaskID, "a RUNNING sibling must not be resolved out from under its container")
	}
	st, ok := rs.TaskState(gate)
	require.True(t, ok)
	require.Equal(t, TaskStatusRunning, st.Status)

	require.Len(t, res.Skipped, 1, "only the pending sibling is cancelled")
	require.Equal(t, x, res.Skipped[0].TaskID)
}

// TestRunState_SyncStartedFromRowsUsesRuntimeID pins the durable marker: a
// claimed row with no runtime_id has not started, whatever its status says.
// started_at cannot be used — ClaimTaskForDispatch stamps it at claim time.
func TestRunState_SyncStartedFromRowsUsesRuntimeID(t *testing.T) {
	b := newTopoBuilder()
	producer := b.task("")
	fanned := b.task("")
	rs := NewRunState(b.build(), 0)

	rs.ApplyCompletion(producer, TaskStatusSucceeded, nil)
	insts := fannedGroup(t, rs, fanned, "process", 0, strParts("bad", "gate"))
	bad, gate := insts[0], insts[1]
	rs.MarkDispatched(bad, "node-1", 1, 0)
	rs.MarkDispatched(gate, "node-1", 1, 0)
	rs.MarkStarted(gate) // stale: the row below says otherwise

	claimedAt := time.Now().UTC()
	rs.SyncStartedFromRows([]models.TaskRun{
		{ID: bad, TaskID: fanned, Status: string(TaskStatusRunning), ClaimedBy: "node-1", RuntimeID: "container-bad", StartedAt: &claimedAt},
		{ID: gate, TaskID: fanned, Status: string(TaskStatusRunning), ClaimedBy: "node-1", RuntimeID: "", StartedAt: &claimedAt},
	})

	badState, _ := rs.TaskState(bad)
	gateState, _ := rs.TaskState(gate)
	require.True(t, badState.Started, "a row carrying a runtime id is running")
	require.False(t, gateState.Started,
		"a claimed row with no runtime id has not started, even though ClaimTaskForDispatch stamped started_at")

	res := rs.ApplyCompletion(bad, TaskStatusFailed, nil)
	require.Len(t, res.Skipped, 1)
	require.Equal(t, gate, res.Skipped[0].TaskID)
}

// TestOwnerManager_FailFastCancelsClaimedUnstartedSibling drives the owner lane
// end to end and asserts the durable row: the cancelled sibling is skipped with
// its own sequence AND its claim revoked, which is what stops the worker that
// still holds it from starting the container.
func TestOwnerManager_FailFastCancelsClaimedUnstartedSibling(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	now := time.Now().UTC()
	trigger := &models.Trigger{ID: uuid.New(), Alias: "ffp-trig-" + uuid.NewString()[:8], Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(trigger).Error)
	job := &models.Job{ID: uuid.New(), Alias: "ffp-job-" + uuid.NewString()[:8], TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(job).Error)

	runRecord, err := store.Start(job.ID, &trigger.ID)
	require.NoError(t, err)
	runID := runRecord.ID

	atomModel := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(atomModel).Error)

	producer := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atomModel.ID, Name: "list", Position: 0, CreatedAt: now, UpdatedAt: now}
	process := &models.Task{
		ID: uuid.New(), JobID: job.ID, AtomID: atomModel.ID, Name: "process", Position: 1,
		FanOutConfig: datatypes.JSON([]byte(`{"from":"list"}`)),
		CreatedAt:    now, UpdatedAt: now,
	}
	require.NoError(t, db.Create([]*models.Task{producer, process}).Error)
	require.NoError(t, db.Create(&models.TaskEdge{ID: uuid.New(), JobID: job.ID, FromTaskID: producer.ID, ToTaskID: process.ID, CreatedAt: now}).Error)

	mk := func(task *models.Task, claimedBy string, outstanding int) {
		require.NoError(t, db.Create(&models.TaskRun{
			ID: uuid.New(), JobRunID: runID, TaskID: task.ID, AtomID: atomModel.ID,
			Engine: atomModel.Engine, Image: atomModel.Image, Command: atomModel.Command,
			Status: string(TaskStatusPending), ClaimedBy: claimedBy, Attempt: 1, MaxAttempts: 1,
			OutstandingPredecessors: outstanding, CreatedAt: now, UpdatedAt: now,
		}).Error)
	}
	mk(producer, "node-1", 0)
	mk(process, "", 1)

	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	require.NoError(t, mgr.Adopt(runID, 1))
	mgr.MarkDispatched(runID, producer.ID, "node-1", 1, 0)

	_, err = mgr.CompleteInstance(runID, producer.ID, uuid.Nil, TaskStatusSucceeded, "success", "", "node-1", nil, nil,
		strParts("bad", "gate", "x"))
	require.NoError(t, err)

	byKey := map[string]models.TaskRun{}
	var rows []models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runID, process.ID).Find(&rows).Error)
	for _, r := range rows {
		byKey[r.PartitionValue] = r
	}
	require.Len(t, byKey, 3)

	// One worker slot: both `bad` and `gate` were dispatched and claimed; only
	// `bad` got a container. `gate` is queued behind it.
	claimedAt := now
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", byKey["bad"].ID).Updates(map[string]any{
		"status": string(TaskStatusRunning), "claimed_by": "worker-1", "runtime_id": "container-bad", "started_at": claimedAt,
	}).Error)
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", byKey["gate"].ID).Updates(map[string]any{
		"status": string(TaskStatusRunning), "claimed_by": "worker-1", "runtime_id": "", "started_at": claimedAt,
	}).Error)
	mgr.MarkDispatched(runID, byKey["bad"].ID, "worker-1", 1, 0)
	mgr.MarkDispatched(runID, byKey["gate"].ID, "worker-1", 1, 0)

	res, err := mgr.CompleteInstance(runID, process.ID, byKey["bad"].ID, TaskStatusFailed, "failure", "boom", "worker-1", nil, nil, nil)
	require.NoError(t, err)
	require.Empty(t, res.Ready, "fail_fast must not release further work")

	after := map[string]models.TaskRun{}
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runID, process.ID).Find(&rows).Error)
	for _, r := range rows {
		after[r.PartitionValue] = r
	}

	require.Equal(t, string(TaskStatusFailed), after["bad"].Status)
	for _, key := range []string{"gate", "x"} {
		require.Equal(t, string(TaskStatusSkipped), after[key].Status,
			"sibling %q had not started and must be cancelled", key)
		require.Equal(t, "fan-out group failed fast", after[key].Error)
		require.Greater(t, after[key].TerminalSequence, int64(0),
			"a skip invisible to replay would be re-dispatched after a takeover; partition %s", key)
	}
	require.Empty(t, after["gate"].ClaimedBy,
		"the cancel must revoke the claim, or the worker still holding it starts the container anyway")
	require.Nil(t, after["gate"].StartedAt,
		"a task that never started must not report a start time")
	require.Empty(t, mgr.ReadyForDispatch(runID), "no sibling may remain dispatchable")

	// The owner-lane cancel must also close the in-flight-RPC window, the same
	// way the SQL lane's does (TestClaimTaskForDispatchRejectsACancelledInstance):
	// a dispatch already on the wire when fail_fast landed reaches
	// HandleDispatch, which claims through ClaimTaskForDispatch and answers 409
	// ReasonTaskNotRunning on ErrTaskClaimMismatch — before it submits anything
	// to a worker (internal/dispatch/dispatch.go).
	require.ErrorIs(t,
		store.ClaimTaskForDispatch(runID, after["gate"].ID, "worker-2", 1, time.Minute, true),
		ErrTaskClaimMismatch,
		"a sibling the owner cancelled must not be claimable by an in-flight dispatch")

	var recheck models.TaskRun
	require.NoError(t, db.First(&recheck, "id = ?", after["gate"].ID).Error)
	require.Equal(t, string(TaskStatusSkipped), recheck.Status, "the rejected claim must leave the row alone")
	require.Empty(t, recheck.ClaimedBy)
	require.Nil(t, recheck.StartedAt)
}

// --- SQL lane -------------------------------------------------------------

// prestartFailFastFixture is failFastFixture's claimed-but-unstarted variant:
// `bad` and `gate` are both claimed by the same worker, but only `bad` has a
// container. This is the one-slot ordering the distributed CI lane hit.
func prestartFailFastFixture(t *testing.T, fo *jobdefschema.FanOut) (*fanOutFixture, map[string]models.TaskRun) {
	t.Helper()
	f := newFanOutFixture(t, fo)
	_, err := f.expand(t, []pkgtask.Partition{
		{Key: "bad"},
		{Key: "gate"},
		{Key: "x", DependsOn: []string{"gate"}},
	})
	require.NoError(t, err)

	byKey := instancesByPartition(t, f)
	require.Len(t, byKey, 3)

	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", byKey["bad"].ID).Updates(map[string]any{
		"status": string(TaskStatusRunning), "claimed_by": "worker-1", "runtime_id": "container-bad",
	}).Error)
	// Claimed, lease held, queued behind `bad` in the same one-slot pool: the
	// claim already flipped the row to running, but no container exists.
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", byKey["gate"].ID).Updates(map[string]any{
		"status": string(TaskStatusRunning), "claimed_by": "worker-1", "runtime_id": "",
		"claim_expires_at": time.Now().UTC().Add(5 * time.Minute),
	}).Error)
	return f, byKey
}

// TestFailFastCancelsClaimedUnstartedSiblingOnCompletionRoute is the SQL lane's
// half: the claimed-but-unstarted sibling must be resolved, with its lease
// released, on the route a worker actually takes (a non-zero container exit
// arrives as CompleteTaskClaimed with result=failure).
func TestFailFastCancelsClaimedUnstartedSiblingOnCompletionRoute(t *testing.T) {
	f, byKey := prestartFailFastFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})

	require.NoError(t, f.store.CompleteTaskClaimed(
		f.runID, byKey["bad"].ID, "failure", "worker-1", nil, nil))

	after := instancesByPartition(t, f)
	assert.Equal(t, string(TaskStatusFailed), after["bad"].Status)

	require.Equal(t, string(TaskStatusSkipped), after["gate"].Status,
		"a claimed sibling with no container must be cancelled before it can start")
	assert.Equal(t, "fan-out group failed fast", after["gate"].Error)
	require.NotZero(t, after["gate"].TerminalSequence)
	assert.Empty(t, after["gate"].ClaimedBy, "the cancel must release the claim")
	assert.Nil(t, after["gate"].ClaimExpiresAt, "a released claim leaves no dangling lease")
	assert.Nil(t, after["gate"].StartedAt)

	assert.Equal(t, string(TaskStatusSkipped), after["x"].Status, "the pending sibling is cancelled as before")

	// The group is fully terminal now, which is what releases the fan-in.
	assert.ElementsMatch(t, []string{"gate", "x"}, taskSkippedPartitions(t, f))
}

// TestFailFastLeavesStartedSiblingAloneOnCompletionRoute pins the boundary: a
// sibling whose container is up keeps running. Without this the change would
// have traded a late start for a terminal row contradicted by a live worker.
func TestFailFastLeavesStartedSiblingAloneOnCompletionRoute(t *testing.T) {
	f, byKey := prestartFailFastFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", byKey["gate"].ID).
		Update("runtime_id", "container-gate").Error)

	require.NoError(t, f.store.CompleteTaskClaimed(
		f.runID, byKey["bad"].ID, "failure", "worker-1", nil, nil))

	after := instancesByPartition(t, f)
	assert.Equal(t, string(TaskStatusRunning), after["gate"].Status,
		"a started sibling must not be resolved out from under its container")
	assert.Equal(t, "worker-1", after["gate"].ClaimedBy, "and must keep its claim")
	assert.Equal(t, string(TaskStatusSkipped), after["x"].Status)
}

// TestCancelBeforeStartLosesToAConcurrentStart pins the guarded UPDATE: the
// pre-start test lives in the WHERE clause, so a worker that creates the
// container between the sibling read and the cancel write makes the cancel a
// no-op rather than stranding a live container with a terminal row.
func TestCancelBeforeStartLosesToAConcurrentStart(t *testing.T) {
	f, byKey := prestartFailFastFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})

	// A stale read: the caller holds the row as it was before the worker started
	// it, exactly what failFastSkipSiblingsTx's SELECT would have.
	stale := byKey["gate"]
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", stale.ID).
		Update("runtime_id", "container-gate").Error)

	var events []event.Event
	var counts dbWriteCounts
	var cancelled bool
	require.NoError(t, f.db.Transaction(func(tx *gorm.DB) error {
		var err error
		cancelled, err = f.store.markInstanceCancelledBeforeStartTx(tx, f.runID, &stale, "fan-out group failed fast", &events, &counts)
		return err
	}))
	assert.False(t, cancelled, "the cancel must lose to a start that already committed")
	assert.Empty(t, events, "and must not emit a task_skipped event for a live task")

	after := instancesByPartition(t, f)
	assert.Equal(t, string(TaskStatusRunning), after["gate"].Status)
	assert.Equal(t, "worker-1", after["gate"].ClaimedBy)
}

// TestClaimTaskForDispatchRejectsACancelledInstance closes the owner-push half
// of the same race: the dispatch RPC can be in flight when fail_fast cancels its
// target. ClaimTaskForDispatch requires a PENDING row, so the claim fails and
// HandleDispatch answers 409 (ReasonTaskNotRunning) before it ever reaches a
// worker — no submission, no state written, and the dispatch loop just leaves
// the task alone.
func TestClaimTaskForDispatchRejectsACancelledInstance(t *testing.T) {
	f, byKey := prestartFailFastFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})

	// `x` is still pending; cancel it the way fail_fast does, then try to
	// dispatch it as an in-flight RPC would.
	var events []event.Event
	var counts dbWriteCounts
	target := byKey["x"]
	require.NoError(t, f.db.Transaction(func(tx *gorm.DB) error {
		_, err := f.store.markInstanceCancelledBeforeStartTx(tx, f.runID, &target, "fan-out group failed fast", &events, &counts)
		return err
	}))

	err := f.store.ClaimTaskForDispatch(f.runID, byKey["x"].ID, "worker-2", 1, time.Minute, true)
	require.ErrorIs(t, err, ErrTaskClaimMismatch,
		"a cancelled instance must not be claimable for dispatch")

	after := instancesByPartition(t, f)
	assert.Equal(t, string(TaskStatusSkipped), after["x"].Status, "the rejected claim must leave the row alone")
	assert.Empty(t, after["x"].ClaimedBy)
	assert.Nil(t, after["x"].StartedAt)
}
