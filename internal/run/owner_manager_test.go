package run

import (
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// seedTwoTaskRun creates a job with tasks a -> b (edge) and pending task_runs,
// returning the run ID and the two task IDs.  Task a is claimed by claimedBy.
func seedTwoTaskRun(t *testing.T, db *gorm.DB, store *Store, claimedBy string) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	now := time.Now().UTC()
	trigger := &models.Trigger{ID: uuid.New(), Alias: "om-trig-" + uuid.NewString()[:8], Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(trigger).Error)
	job := &models.Job{ID: uuid.New(), Alias: "om-job-" + uuid.NewString()[:8], TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(job).Error)
	runRecord, err := store.Start(job.ID, &trigger.ID)
	require.NoError(t, err)
	atom := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo","x"]`, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(atom).Error)

	a := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atom.ID, Name: "a", Position: 0, CreatedAt: now, UpdatedAt: now}
	b := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atom.ID, Name: "b", Position: 1, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create([]*models.Task{a, b}).Error)
	require.NoError(t, db.Create(&models.TaskEdge{ID: uuid.New(), JobID: job.ID, FromTaskID: a.ID, ToTaskID: b.ID, CreatedAt: now}).Error)

	mk := func(task *models.Task, claimed string, outstanding int) {
		require.NoError(t, db.Create(&models.TaskRun{
			ID: uuid.New(), JobRunID: runRecord.ID, TaskID: task.ID, AtomID: atom.ID,
			Engine: atom.Engine, Image: atom.Image, Command: atom.Command,
			Status: string(TaskStatusPending), ClaimedBy: claimed, Attempt: 1, MaxAttempts: 1,
			OutstandingPredecessors: outstanding, CreatedAt: now, UpdatedAt: now,
		}).Error)
	}
	mk(a, claimedBy, 0)
	mk(b, "", 1)
	return runRecord.ID, a.ID, b.ID
}

func TestOwnerManager_AdoptCompleteAdvancesAndCheckpoints(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, taskA, taskB := seedTwoTaskRun(t, db, store, "node-1")

	// Events:1 so every completion triggers a checkpoint (easy to assert).
	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	require.NoError(t, mgr.Adopt(runID, 1))

	ready := mgr.Ready(runID)
	require.Equal(t, []uuid.UUID{taskA}, ready, "root task a should be ready after adopt")

	res, err := mgr.Complete(runID, taskA, TaskStatusSucceeded, "success", "", "node-1", nil, nil)
	require.NoError(t, err)
	require.True(t, res.Owned)
	require.False(t, res.Complete)
	require.Equal(t, []uuid.UUID{taskB}, res.Ready, "completing a should ready b")

	// a's terminal row is durably written with terminal_sequence stamped.
	var aRow models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runID, taskA).First(&aRow).Error)
	require.Equal(t, string(TaskStatusSucceeded), aRow.Status)
	require.Equal(t, int64(1), aRow.TerminalSequence)

	// A checkpoint was written (Events:1 cadence).
	cp, err := store.LatestFullCheckpoint(runID)
	require.NoError(t, err)
	require.NotNil(t, cp, "a checkpoint should have been written")

	// Completing b finishes the run.
	res, err = mgr.Complete(runID, taskB, TaskStatusSucceeded, "success", "", "", nil, nil)
	require.NoError(t, err)
	require.True(t, res.Complete, "run should be complete after b")
}

// TestOwnerManager_RecoverThenLoopFlow mimics the live wiring exactly: the
// dispatch loop Recovers an owned run (not Adopt), pulls ReadyForDispatch,
// MarkDispatched, then a completion arrives via Complete.  This reproduces the
// production path to catch wiring/persistence bugs the Adopt-based test misses.
func TestOwnerManager_RecoverThenLoopFlow(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, taskA, taskB := seedTwoTaskRun(t, db, store, "node-1")

	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})

	// Loop tick 1: not owned yet → Recover, then dispatch the ready root.
	require.False(t, mgr.Owns(runID))
	_, err := mgr.Recover(runID, 1)
	require.NoError(t, err)
	require.True(t, mgr.Owns(runID), "Recover must persist the run in the manager")

	ready := mgr.ReadyForDispatch(runID)
	require.Len(t, ready, 1)
	require.Equal(t, taskA, ready[0].TaskID, "root a should be ready after recover")

	mgr.MarkDispatched(runID, taskA, "node-1", 1, 0)
	require.Empty(t, mgr.ReadyForDispatch(runID), "dispatched root must leave the ready queue")

	// Loop tick 2: still owned → must NOT re-Recover (which would discard state).
	require.True(t, mgr.Owns(runID), "run must stay owned across ticks")

	// Completion arrives at HandleComplete → Complete.
	res, err := mgr.Complete(runID, taskA, TaskStatusSucceeded, "success", "", "node-1", nil, nil)
	require.NoError(t, err)
	require.True(t, res.Owned, "the run is owned, completion must take the in-memory path")
	require.Equal(t, []uuid.UUID{taskB}, res.Ready, "completing a must advance and ready b")

	var aRow models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runID, taskA).First(&aRow).Error)
	require.Equal(t, string(TaskStatusSucceeded), aRow.Status)
	require.Greater(t, aRow.TerminalSequence, int64(0), "owner completion must stamp a terminal_sequence > 0")
}

// TestOwnerManager_ConcurrentRunsNoDeadlock exercises several runs through the
// manager concurrently (adopt, ready, dispatch, complete) to validate the
// per-run-mutex design: independent runs must proceed in parallel without
// deadlock or data race (run with -race).
func TestOwnerManager_ConcurrentRunsNoDeadlock(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})

	const runs = 8
	type runInfo struct {
		runID, a, b uuid.UUID
	}
	infos := make([]runInfo, runs)
	for i := range infos {
		rid, a, b := seedTwoTaskRun(t, db, store, "node-1")
		// Simulate both tasks having been claimed by this node (HandleDispatch
		// does this in the real flow) so the owner-completion claim fence passes.
		require.NoError(t, db.Model(&models.TaskRun{}).Where("job_run_id = ?", rid).Update("claimed_by", "node-1").Error)
		infos[i] = runInfo{rid, a, b}
		require.NoError(t, mgr.Adopt(rid, 1))
	}

	var wg sync.WaitGroup
	for _, info := range infos {
		wg.Add(1)
		go func(info runInfo) {
			defer wg.Done()
			// Drive the run to completion through the manager API concurrently.
			_ = mgr.ReadyForDispatch(info.runID)
			mgr.MarkDispatched(info.runID, info.a, "node-1", 1, 0)
			if _, err := mgr.Complete(info.runID, info.a, TaskStatusSucceeded, "success", "", "node-1", nil, nil); err != nil {
				t.Errorf("complete a: %v", err)
			}
			mgr.MarkDispatched(info.runID, info.b, "node-1", 1, 0)
			if _, err := mgr.Complete(info.runID, info.b, TaskStatusSucceeded, "success", "", "node-1", nil, nil); err != nil {
				t.Errorf("complete b: %v", err)
			}
		}(info)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent run operations deadlocked")
	}

	// Every run finished and was dropped on completion.
	for _, info := range infos {
		require.False(t, mgr.Owns(info.runID), "completed run should be dropped: %s", info.runID)
	}
}

func TestOwnerManager_CompleteUnownedRunFallsBack(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	mgr := NewOwnerManager(store, CheckpointConfig{})

	res, err := mgr.Complete(uuid.New(), uuid.New(), TaskStatusSucceeded, "success", "", "n", nil, nil)
	require.NoError(t, err)
	require.False(t, res.Owned, "an unowned run must report Owned=false so the caller falls back")
}
