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

func TestOwnerManager_CompleteUnownedRunFallsBack(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	mgr := NewOwnerManager(store, CheckpointConfig{})

	res, err := mgr.Complete(uuid.New(), uuid.New(), TaskStatusSucceeded, "success", "", "n", nil, nil)
	require.NoError(t, err)
	require.False(t, res.Owned, "an unowned run must report Owned=false so the caller falls back")
}
