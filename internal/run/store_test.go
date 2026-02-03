package run

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestStorePersistsRunState(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)

	jobID := uuid.New()
	runRecord, err := store.Start(jobID)
	require.NoError(t, err)
	require.Equal(t, StatusRunning, runRecord.Status)

	atomA := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine",
		Command: `["echo","a"]`,
	}
	atomB := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine",
		Command: `["echo","b"]`,
	}
	require.NoError(t, db.Create(atomA).Error)
	require.NoError(t, db.Create(atomB).Error)

	taskA := &models.Task{
		ID:     uuid.New(),
		JobID:  jobID,
		AtomID: atomA.ID,
	}
	taskB := &models.Task{
		ID:     uuid.New(),
		JobID:  jobID,
		AtomID: atomB.ID,
	}
	require.NoError(t, db.Create(taskA).Error)
	require.NoError(t, db.Create(taskB).Error)

	edge := &models.TaskEdge{
		ID:         uuid.New(),
		JobID:      jobID,
		FromTaskID: taskA.ID,
		ToTaskID:   taskB.ID,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	require.NoError(t, db.Create(edge).Error)

	require.NoError(t, store.RegisterTask(runRecord.ID, taskA, atomA, 0))
	require.NoError(t, store.RegisterTask(runRecord.ID, taskB, atomB, 1))

	state, err := store.Get(runRecord.ID)
	require.NoError(t, err)
	require.Len(t, state.Tasks, 2)

	var taskStateA, taskStateB *TaskRun
	for _, rt := range state.Tasks {
		switch rt.ID {
		case taskA.ID:
			taskStateA = rt
		case taskB.ID:
			taskStateB = rt
		}
	}
	require.NotNil(t, taskStateA)
	require.NotNil(t, taskStateB)
	require.Equal(t, TaskStatusPending, taskStateA.Status)
	require.Equal(t, 1, taskStateB.OutstandingPredecessors)

	require.NoError(t, store.StartTask(runRecord.ID, taskA.ID, "runtime-a"))

	// Simulate restart and ensure running tasks reset back to pending.
	secondStore := NewStore(db)
	require.NoError(t, secondStore.ResetInFlightTasks(runRecord.ID))

	// Completing the first task should decrement the successor outstanding count.
	require.NoError(t, store.CompleteTask(runRecord.ID, taskA.ID, "ok"))

	state, err = store.Get(runRecord.ID)
	require.NoError(t, err)
	for _, rt := range state.Tasks {
		if rt.ID == taskB.ID {
			require.Equal(t, 0, rt.OutstandingPredecessors)
		}
	}

	require.NoError(t, store.CompleteTask(runRecord.ID, taskB.ID, "done"))
	require.NoError(t, store.Complete(runRecord.ID, nil))

	finalStore := NewStore(db)
	finalRun, err := finalStore.Get(runRecord.ID)
	require.NoError(t, err)
	require.Equal(t, StatusSucceeded, finalRun.Status)
}

func TestCompleteTaskSkipsFallbackWhenJobHasEdges(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)

	jobID := uuid.New()
	runRecord, err := store.Start(jobID)
	require.NoError(t, err)

	atomA := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine",
		Command: `["echo","a"]`,
	}
	atomB := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine",
		Command: `["echo","b"]`,
	}
	atomC := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine",
		Command: `["echo","c"]`,
	}
	require.NoError(t, db.Create(atomA).Error)
	require.NoError(t, db.Create(atomB).Error)
	require.NoError(t, db.Create(atomC).Error)

	base := time.Now().UTC().Add(-2 * time.Minute)
	taskA := &models.Task{
		ID:        uuid.New(),
		JobID:     jobID,
		AtomID:    atomA.ID,
		CreatedAt: base,
		UpdatedAt: base,
	}
	taskC := &models.Task{
		ID:        uuid.New(),
		JobID:     jobID,
		AtomID:    atomC.ID,
		CreatedAt: base.Add(10 * time.Second),
		UpdatedAt: base.Add(10 * time.Second),
	}
	taskB := &models.Task{
		ID:        uuid.New(),
		JobID:     jobID,
		AtomID:    atomB.ID,
		CreatedAt: base.Add(20 * time.Second),
		UpdatedAt: base.Add(20 * time.Second),
	}
	require.NoError(t, db.Create(taskA).Error)
	require.NoError(t, db.Create(taskB).Error)
	require.NoError(t, db.Create(taskC).Error)

	edge := &models.TaskEdge{
		ID:         uuid.New(),
		JobID:      jobID,
		FromTaskID: taskA.ID,
		ToTaskID:   taskB.ID,
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
	}
	require.NoError(t, db.Create(edge).Error)

	require.NoError(t, store.RegisterTask(runRecord.ID, taskA, atomA, 0))
	require.NoError(t, store.RegisterTask(runRecord.ID, taskB, atomB, 1))
	require.NoError(t, store.RegisterTask(runRecord.ID, taskC, atomC, 0))

	require.NoError(t, store.CompleteTask(runRecord.ID, taskC.ID, "done"))

	state, err := store.Get(runRecord.ID)
	require.NoError(t, err)
	for _, rt := range state.Tasks {
		if rt.ID == taskB.ID {
			require.Equal(t, 1, rt.OutstandingPredecessors)
		}
	}

	require.NoError(t, store.CompleteTask(runRecord.ID, taskA.ID, "done"))

	state, err = store.Get(runRecord.ID)
	require.NoError(t, err)
	for _, rt := range state.Tasks {
		if rt.ID == taskB.ID {
			require.Equal(t, 0, rt.OutstandingPredecessors)
		}
	}
}
