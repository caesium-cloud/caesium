package run

import (
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// seedHaltOwnerRun is the reviewer's reproduction on PR #456: a failed branch
// plus an unstarted independent all_success step leading to an all_done
// cleanup, driven through the in-memory run owner.
//
//	fail (root) ──▶ cleanup (all_done)
//	independent (root, all_success) ──▶ indep-cleanup (all_done)
func seedHaltOwnerRun(t *testing.T, db *gorm.DB, store *Store) (uuid.UUID, map[string]uuid.UUID) {
	t.Helper()
	now := time.Now().UTC()

	trigger := &models.Trigger{ID: uuid.New(), Alias: "halt-owner-trig-" + uuid.NewString()[:8], Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(trigger).Error)
	job := &models.Job{ID: uuid.New(), Alias: "halt-owner-job-" + uuid.NewString()[:8], TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(job).Error)
	runRecord, err := store.Start(job.ID, &trigger.ID)
	require.NoError(t, err)
	atomModel := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(atomModel).Error)

	ids := map[string]uuid.UUID{}
	mk := func(name, rule string, position int) *models.Task {
		task := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atomModel.ID, Name: name, Position: position, TriggerRule: rule, CreatedAt: now, UpdatedAt: now}
		require.NoError(t, db.Create(task).Error)
		ids[name] = task.ID
		return task
	}
	fail := mk("fail", jobdefschema.TriggerRuleAllSuccess, 0)
	independent := mk("independent", jobdefschema.TriggerRuleAllSuccess, 1)
	cleanup := mk("cleanup", jobdefschema.TriggerRuleAllDone, 2)
	indepCleanup := mk("indep-cleanup", jobdefschema.TriggerRuleAllDone, 3)
	require.NoError(t, db.Create([]*models.TaskEdge{
		{ID: uuid.New(), JobID: job.ID, FromTaskID: fail.ID, ToTaskID: cleanup.ID, CreatedAt: now},
		{ID: uuid.New(), JobID: job.ID, FromTaskID: independent.ID, ToTaskID: indepCleanup.ID, CreatedAt: now},
	}).Error)
	require.NoError(t, store.RegisterTasks(runRecord.ID, []RegisterTaskInput{
		{Task: fail, Atom: atomModel, OutstandingPredecessors: 0},
		{Task: independent, Atom: atomModel, OutstandingPredecessors: 0},
		{Task: cleanup, Atom: atomModel, OutstandingPredecessors: 1},
		{Task: indepCleanup, Atom: atomModel, OutstandingPredecessors: 1},
	}))
	// The owner's completion write is fenced on the worker's claim
	// (CompleteTaskOwner); stamp the rows the way seedChainRun does.
	require.NoError(t, db.Model(&models.TaskRun{}).Where("job_run_id = ?", runRecord.ID).Update("claimed_by", "node-1").Error)
	return runRecord.ID, ids
}

func haltOwnerRow(t *testing.T, db *gorm.DB, runID, taskID uuid.UUID) models.TaskRun {
	t.Helper()
	var row models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&row).Error)
	return row
}

// TestHaltUnstartedTasksSynchronizesInMemoryRunOwner pins the P1 from the
// review of #456. With CAESIUM_RUN_OWNER_IN_MEMORY=true the dispatch loop
// reads ReadyForDispatch from the owner's RunState, which consumes neither
// task_skipped events nor the row scalar. The halt sweep resolves rows in SQL
// from a worker (or the waiter), so before the fix the halted `independent`
// stayed on the owner's ready queue — every dispatch of it refused, the row
// already skipped — and `indep-cleanup`, which the sweep released in SQL,
// never entered the queue: the waiter (reading the rows) kept the run alive
// for a step nobody would dispatch.
func TestHaltUnstartedTasksSynchronizesInMemoryRunOwner(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, ids := seedHaltOwnerRun(t, db, store)

	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	require.NoError(t, mgr.Adopt(runID, 1))

	// The failure arrives through the owner, as it does on that lane.
	mgr.MarkDispatched(runID, ids["fail"], "node-1", 1, 0)
	_, err := mgr.Complete(runID, ids["fail"], TaskStatusFailed, "failure", "boom", "node-1", nil, nil)
	require.NoError(t, err)
	require.Equal(t, string(TaskStatusFailed), haltOwnerRow(t, db, runID, ids["fail"]).Status)
	require.ElementsMatch(t, []uuid.UUID{ids["independent"], ids["cleanup"]}, mgr.Ready(runID),
		"precondition: the owner has released the failure's all_done successor and still holds the independent root")

	// The worker's halt sweep resolves the rows in SQL.
	skipped, err := store.HaltUnstartedTasks(runID, ids["fail"], nil)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{ids["independent"]}, skipped)
	independent := haltOwnerRow(t, db, runID, ids["independent"])
	require.Equal(t, string(TaskStatusSkipped), independent.Status)
	require.Equal(t, HaltReason("fail"), independent.Error)
	require.Zero(t, haltOwnerRow(t, db, runID, ids["indep-cleanup"]).OutstandingPredecessors,
		"the sweep released the cleanup in SQL")

	// The owner's view must agree with the rows: the swept row is gone from
	// the ready queue and the released cleanup is on it.
	ready := mgr.Ready(runID)
	require.NotContains(t, ready, ids["independent"],
		"a halted step must leave the owner's ready queue (the durable row is skipped)")
	require.Contains(t, ready, ids["indep-cleanup"],
		"the all_done cleanup the sweep released must enter the owner's ready queue")
	require.Contains(t, ready, ids["cleanup"])

	ts, ok := mgr.get(runID)
	require.True(t, ok)
	ts.mu.Lock()
	state, known := ts.state.TaskState(ids["independent"])
	seq := ts.state.Sequence()
	ts.mu.Unlock()
	require.True(t, known)
	require.Equal(t, TaskStatusSkipped, state.Status)
	require.GreaterOrEqual(t, seq, independent.TerminalSequence,
		"the owner adopts the sweep's terminal_sequence so a takeover replays the tail in order")

	// Running both cleanups through the owner completes the run: every task
	// is terminal in memory as well as in SQL, and the owner finalizes it.
	for _, name := range []string{"cleanup", "indep-cleanup"} {
		mgr.MarkDispatched(runID, ids[name], "node-1", 1, 0)
		res, err := mgr.Complete(runID, ids[name], TaskStatusSucceeded, "success", "", "node-1", nil, nil)
		require.NoError(t, err)
		if name == "indep-cleanup" {
			require.True(t, res.Complete, "the DAG must be complete in memory once the last cleanup lands")
		}
	}
	jobRun, err := store.Get(runID)
	require.NoError(t, err)
	require.Equal(t, StatusFailed, jobRun.Status, "the halted run ends failed on the upstream failure")
	require.False(t, mgr.Owns(runID), "a finalized run is dropped by the owner")
}

// TestApplyTerminalRowsIgnoresRunsThisNodeDoesNotOwn pins the no-op half: a
// sweep on a node that is not the run's owner touches nothing here — the
// owner's takeover rebuilds from the rows, which already carry the sweep.
func TestApplyTerminalRowsIgnoresRunsThisNodeDoesNotOwn(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, ids := seedHaltOwnerRun(t, db, store)
	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})

	require.NoError(t, store.StartTask(runID, ids["fail"], "fail-container"))
	require.NoError(t, store.FailTask(runID, ids["fail"], errors.New("boom")))
	_, err := store.HaltUnstartedTasks(runID, ids["fail"], nil)
	require.NoError(t, err)
	require.False(t, mgr.Owns(runID))
	require.Nil(t, mgr.Ready(runID))
}
