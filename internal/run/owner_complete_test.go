package run

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestCompleteTaskOwner_WritesTerminalRowsWithoutAdvancing verifies the owner
// durable-write path: it stamps terminal_sequence + owner_generation on the
// completed task and on owner-decided skips, but does NOT decrement a
// successor's predecessor count (advancement is the owner's in-memory job).
func TestCompleteTaskOwner_WritesTerminalRowsWithoutAdvancing(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	now := time.Now().UTC()

	trigger := &models.Trigger{ID: uuid.New(), Alias: "own-trig", Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(trigger).Error)
	job := &models.Job{ID: uuid.New(), Alias: "own-job", TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(job).Error)
	runRecord, err := store.Start(job.ID, &trigger.ID)
	require.NoError(t, err)
	atom := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo","x"]`, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(atom).Error)

	taskA := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atom.ID, Name: "a", CreatedAt: now, UpdatedAt: now}
	taskB := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atom.ID, Name: "b", CreatedAt: now, UpdatedAt: now}
	taskC := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atom.ID, Name: "c", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create([]*models.Task{taskA, taskB, taskC}).Error)

	mkRun := func(task *models.Task, status string, claimedBy string, outstanding int) {
		require.NoError(t, db.Create(&models.TaskRun{
			ID: uuid.New(), JobRunID: runRecord.ID, TaskID: task.ID, AtomID: atom.ID,
			Engine: atom.Engine, Image: atom.Image, Command: atom.Command,
			Status: status, ClaimedBy: claimedBy, Attempt: 1, MaxAttempts: 1,
			OutstandingPredecessors: outstanding, CreatedAt: now, UpdatedAt: now,
		}).Error)
	}
	mkRun(taskA, string(TaskStatusRunning), "node-1", 0) // claimed + running
	mkRun(taskB, string(TaskStatusPending), "", 1)       // will be owner-skipped
	mkRun(taskC, string(TaskStatusPending), "", 1)       // successor; must NOT be decremented

	err = store.CompleteTaskOwner(
		runRecord.ID, taskA.ID, TaskStatusSucceeded, "success", "", "node-1",
		map[string]string{"k": "v"}, nil,
		5, 7,
		[]OwnerSkip{{TaskID: taskB.ID, TerminalSequence: 6, Reason: "branch not selected"}},
	)
	require.NoError(t, err)

	get := func(taskID uuid.UUID) models.TaskRun {
		var tr models.TaskRun
		require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runRecord.ID, taskID).First(&tr).Error)
		return tr
	}

	a := get(taskA.ID)
	require.Equal(t, string(TaskStatusSucceeded), a.Status)
	require.Equal(t, int64(5), a.TerminalSequence)
	require.Equal(t, int64(7), a.OwnerGeneration)

	b := get(taskB.ID)
	require.Equal(t, string(TaskStatusSkipped), b.Status)
	require.Equal(t, int64(6), b.TerminalSequence)
	require.Equal(t, int64(7), b.OwnerGeneration)

	// The crucial assertion: C was NOT advanced — no SQL predecessor decrement.
	c := get(taskC.ID)
	require.Equal(t, 1, c.OutstandingPredecessors, "owner path must not decrement successors in SQL")
	require.Equal(t, string(TaskStatusPending), c.Status)
}

func TestCompleteTaskOwner_ClaimMismatch(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	now := time.Now().UTC()

	trigger := &models.Trigger{ID: uuid.New(), Alias: "own-trig2", Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(trigger).Error)
	job := &models.Job{ID: uuid.New(), Alias: "own-job2", TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(job).Error)
	runRecord, err := store.Start(job.ID, &trigger.ID)
	require.NoError(t, err)
	atom := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo","x"]`, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(atom).Error)
	task := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atom.ID, Name: "a", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(task).Error)
	require.NoError(t, db.Create(&models.TaskRun{
		ID: uuid.New(), JobRunID: runRecord.ID, TaskID: task.ID, AtomID: atom.ID,
		Engine: atom.Engine, Image: atom.Image, Command: atom.Command,
		Status: string(TaskStatusRunning), ClaimedBy: "node-1", Attempt: 1, MaxAttempts: 1,
		CreatedAt: now, UpdatedAt: now,
	}).Error)

	// A completion from a node that does not hold the claim is rejected.
	err = store.CompleteTaskOwner(runRecord.ID, task.ID, TaskStatusSucceeded, "success", "", "wrong-node", nil, nil, 1, 1, nil)
	require.ErrorIs(t, err, ErrTaskClaimMismatch)
}
