package run

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestStorePersistsRunState(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)

	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)
	require.Equal(t, StatusRunning, runRecord.Status)

	atomA := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","a"]`,
	}
	atomB := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
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
	require.NoError(t, store.CompleteTask(runRecord.ID, taskA.ID, "ok", nil, nil))

	state, err = store.Get(runRecord.ID)
	require.NoError(t, err)
	for _, rt := range state.Tasks {
		if rt.ID == taskB.ID {
			require.Equal(t, 0, rt.OutstandingPredecessors)
		}
	}

	require.NoError(t, store.CompleteTask(runRecord.ID, taskB.ID, "ok", nil, nil))
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
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atomA := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","a"]`,
	}
	atomB := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","b"]`,
	}
	atomC := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
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

	require.NoError(t, store.CompleteTask(runRecord.ID, taskC.ID, "ok", nil, nil))

	state, err := store.Get(runRecord.ID)
	require.NoError(t, err)
	for _, rt := range state.Tasks {
		if rt.ID == taskB.ID {
			require.Equal(t, 1, rt.OutstandingPredecessors)
		}
	}

	require.NoError(t, store.CompleteTask(runRecord.ID, taskA.ID, "ok", nil, nil))

	state, err = store.Get(runRecord.ID)
	require.NoError(t, err)
	for _, rt := range state.Tasks {
		if rt.ID == taskB.ID {
			require.Equal(t, 0, rt.OutstandingPredecessors)
		}
	}
}

func TestRegisterTaskPersistsSchemaValidationConfig(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)
	now := time.Now().UTC()

	trigger := &models.Trigger{
		ID:        uuid.New(),
		Alias:     "schema-validation-trigger",
		Type:      models.TriggerTypeCron,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(trigger).Error)

	job := &models.Job{
		ID:               uuid.New(),
		Alias:            "schema-validation-job",
		TriggerID:        trigger.ID,
		SchemaValidation: jobdef.SchemaValidationFail,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	require.NoError(t, db.Create(job).Error)

	runRecord, err := store.Start(job.ID, &trigger.ID)
	require.NoError(t, err)

	atom := &models.Atom{
		ID:        uuid.New(),
		Engine:    models.AtomEngineDocker,
		Image:     "alpine:3.23",
		Command:   `["echo","test"]`,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(atom).Error)

	schema := datatypes.JSON(`{"type":"object","required":["rows_written"]}`)
	task := &models.Task{
		ID:           uuid.New(),
		JobID:        job.ID,
		AtomID:       atom.ID,
		Name:         "transform",
		OutputSchema: schema,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	require.NoError(t, db.Create(task).Error)

	require.NoError(t, store.RegisterTask(runRecord.ID, task, atom, 0))

	var persisted models.TaskRun
	require.NoError(t, db.First(&persisted, "job_run_id = ? AND task_id = ?", runRecord.ID, task.ID).Error)
	require.JSONEq(t, string(schema), string(persisted.OutputSchema))
	require.Equal(t, job.SchemaValidation, persisted.SchemaValidation)
}

func TestRegisterTasksBatchesReadyEventsAndSkipsExisting(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)
	now := time.Now().UTC()

	trigger := &models.Trigger{
		ID:        uuid.New(),
		Alias:     "batch-trigger",
		Type:      models.TriggerTypeCron,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(trigger).Error)

	job := &models.Job{
		ID:        uuid.New(),
		Alias:     "batch-job",
		TriggerID: trigger.ID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(job).Error)

	runRecord, err := store.Start(job.ID, &trigger.ID)
	require.NoError(t, err)

	atom := &models.Atom{
		ID:        uuid.New(),
		Engine:    models.AtomEngineDocker,
		Image:     "alpine:3.23",
		Command:   `["echo","batch"]`,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(atom).Error)

	taskA := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atom.ID, Name: "a", CreatedAt: now, UpdatedAt: now}
	taskB := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atom.ID, Name: "b", CreatedAt: now, UpdatedAt: now}
	taskC := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atom.ID, Name: "c", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create([]*models.Task{taskA, taskB, taskC}).Error)

	require.NoError(t, db.Create(&models.TaskRun{
		ID:                      uuid.New(),
		JobRunID:                runRecord.ID,
		TaskID:                  taskA.ID,
		AtomID:                  atom.ID,
		Engine:                  atom.Engine,
		Image:                   atom.Image,
		Command:                 atom.Command,
		Status:                  string(TaskStatusPending),
		Attempt:                 1,
		MaxAttempts:             1,
		OutstandingPredecessors: 0,
		CreatedAt:               now,
		UpdatedAt:               now,
	}).Error)

	require.NoError(t, store.RegisterTasks(runRecord.ID, []RegisterTaskInput{
		{Task: taskA, Atom: atom, OutstandingPredecessors: 0},
		{Task: taskB, Atom: atom, OutstandingPredecessors: 0},
		{Task: taskC, Atom: atom, OutstandingPredecessors: 1},
	}))

	var taskRunCount int64
	require.NoError(t, db.Model(&models.TaskRun{}).Where("job_run_id = ?", runRecord.ID).Count(&taskRunCount).Error)
	require.Equal(t, int64(3), taskRunCount)

	var readyEvents []models.ExecutionEvent
	require.NoError(t, db.Where("run_id = ? AND type = ?", runRecord.ID, string(event.TypeTaskReady)).Find(&readyEvents).Error)
	require.Len(t, readyEvents, 1)
	require.NotNil(t, readyEvents[0].TaskID)
	require.NotNil(t, readyEvents[0].JobID)
	require.Equal(t, taskB.ID, *readyEvents[0].TaskID)
	require.Equal(t, job.ID, *readyEvents[0].JobID)
	require.True(t, readyEvents[0].BusDispatchPending)
	require.Nil(t, readyEvents[0].BusDispatchedAt)
}

func TestRegisterTasksReturnsMissingJobRunError(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)
	task := &models.Task{ID: uuid.New(), JobID: uuid.New(), AtomID: uuid.New()}
	atom := &models.Atom{ID: task.AtomID, Engine: models.AtomEngineDocker, Image: "alpine:3.23"}

	err := store.RegisterTasks(uuid.New(), []RegisterTaskInput{
		{Task: task, Atom: atom, OutstandingPredecessors: 0},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "job run")
}

func TestWithStoreBusyRetryRetriesSQLiteContention(t *testing.T) {
	attempts := 0
	err := withStoreBusyRetry(func() error {
		attempts++
		if attempts == 1 {
			return sqlite3.Error{Code: sqlite3.ErrBusy}
		}
		return nil
	})

	require.NoError(t, err)
	require.Equal(t, 2, attempts)
}

func TestWithStoreBusyRetryRetriesCheckpointContention(t *testing.T) {
	attempts := 0
	err := withStoreBusyRetry(func() error {
		attempts++
		if attempts == 1 {
			return errors.New("checkpoint in progress")
		}
		return nil
	})

	require.NoError(t, err)
	require.Equal(t, 2, attempts)
}

func TestClaimAwareTaskLifecycleMethods(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)

	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atom := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","a"]`,
	}
	require.NoError(t, db.Create(atom).Error)

	task := &models.Task{
		ID:     uuid.New(),
		JobID:  jobID,
		AtomID: atom.ID,
	}
	require.NoError(t, db.Create(task).Error)
	require.NoError(t, store.RegisterTask(runRecord.ID, task, atom, 0))

	claimOwner := "node-a"
	require.NoError(t, db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", runRecord.ID, task.ID).
		Updates(map[string]interface{}{
			"status":           string(TaskStatusRunning),
			"claimed_by":       claimOwner,
			"claim_expires_at": time.Now().UTC().Add(1 * time.Minute),
			"claim_attempt":    1,
		}).Error)

	err = store.StartTaskClaimed(runRecord.ID, task.ID, "runtime-a", "node-b")
	require.ErrorIs(t, err, ErrTaskClaimMismatch)

	require.NoError(t, store.StartTaskClaimed(runRecord.ID, task.ID, "runtime-a", claimOwner))

	err = store.CompleteTaskClaimed(runRecord.ID, task.ID, "ok", "node-b", nil, nil)
	require.ErrorIs(t, err, ErrTaskClaimMismatch)

	require.NoError(t, store.CompleteTaskClaimed(runRecord.ID, task.ID, "ok", claimOwner, nil, nil))

	state, err := store.Get(runRecord.ID)
	require.NoError(t, err)

	var taskState *TaskRun
	for _, candidate := range state.Tasks {
		if candidate.ID == task.ID {
			taskState = candidate
			break
		}
	}
	require.NotNil(t, taskState)
	require.Equal(t, TaskStatusSucceeded, taskState.Status)
	require.Equal(t, "runtime-a", taskState.RuntimeID)
	require.Equal(t, claimOwner, taskState.ClaimedBy)
	require.NotNil(t, taskState.StartedAt)
}

func TestCompleteTaskWithOutput(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)

	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atomModel := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","hello"]`,
	}
	require.NoError(t, db.Create(atomModel).Error)

	task := &models.Task{
		ID:     uuid.New(),
		JobID:  jobID,
		AtomID: atomModel.ID,
		Name:   "step-one",
	}
	require.NoError(t, db.Create(task).Error)
	require.NoError(t, store.RegisterTask(runRecord.ID, task, atomModel, 0))
	require.NoError(t, store.StartTask(runRecord.ID, task.ID, "runtime-1"))

	output := map[string]string{
		"row_count": "42",
		"path":      "/data/out.parquet",
	}
	require.NoError(t, store.CompleteTask(runRecord.ID, task.ID, "ok", output, nil))

	state, err := store.Get(runRecord.ID)
	require.NoError(t, err)

	var taskState *TaskRun
	for _, candidate := range state.Tasks {
		if candidate.ID == task.ID {
			taskState = candidate
			break
		}
	}
	require.NotNil(t, taskState)
	require.Equal(t, TaskStatusSucceeded, taskState.Status)
	require.Equal(t, map[string]string{
		"row_count": "42",
		"path":      "/data/out.parquet",
	}, taskState.Output)
}

func TestCompleteTaskWithNilOutput(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)

	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atomModel := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","hello"]`,
	}
	require.NoError(t, db.Create(atomModel).Error)

	task := &models.Task{
		ID:     uuid.New(),
		JobID:  jobID,
		AtomID: atomModel.ID,
	}
	require.NoError(t, db.Create(task).Error)
	require.NoError(t, store.RegisterTask(runRecord.ID, task, atomModel, 0))
	require.NoError(t, store.StartTask(runRecord.ID, task.ID, "runtime-1"))
	require.NoError(t, store.CompleteTask(runRecord.ID, task.ID, "ok", nil, nil))

	state, err := store.Get(runRecord.ID)
	require.NoError(t, err)

	var taskState *TaskRun
	for _, candidate := range state.Tasks {
		if candidate.ID == task.ID {
			taskState = candidate
			break
		}
	}
	require.NotNil(t, taskState)
	require.Equal(t, TaskStatusSucceeded, taskState.Status)
	require.Nil(t, taskState.Output)
}

func TestRetryTaskClearsPreviousExecutionArtifacts(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)

	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atomModel := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","hello"]`,
	}
	require.NoError(t, db.Create(atomModel).Error)

	task := &models.Task{
		ID:     uuid.New(),
		JobID:  jobID,
		AtomID: atomModel.ID,
		Name:   "retry-me",
	}
	require.NoError(t, db.Create(task).Error)
	require.NoError(t, store.RegisterTask(runRecord.ID, task, atomModel, 0))
	require.NoError(t, store.StartTask(runRecord.ID, task.ID, "runtime-1"))
	require.NoError(t, store.CompleteTask(runRecord.ID, task.ID, "failure", map[string]string{"rows": "10"}, []string{"branch-a"}))
	require.NoError(t, store.SaveTaskLogSnapshot(runRecord.ID, task.ID, &TaskLogSnapshot{
		Text:      "previous attempt logs",
		Truncated: true,
	}))

	require.NoError(t, store.RetryTask(runRecord.ID, task.ID, 2))

	state, err := store.Get(runRecord.ID)
	require.NoError(t, err)

	var taskState *TaskRun
	for _, candidate := range state.Tasks {
		if candidate.ID == task.ID {
			taskState = candidate
			break
		}
	}
	require.NotNil(t, taskState)
	require.Equal(t, TaskStatusPending, taskState.Status)
	require.Equal(t, "", taskState.RuntimeID)
	require.Equal(t, "", taskState.Result)
	require.Nil(t, taskState.Output)

	snapshot, err := store.GetTaskLogSnapshot(runRecord.ID, task.ID)
	require.NoError(t, err)
	require.Nil(t, snapshot)

	var model models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runRecord.ID, task.ID).First(&model).Error)
	require.Equal(t, "", model.LogText)
	require.False(t, model.LogTruncated)
	require.Len(t, model.BranchSelections, 0)
}

func TestPredecessorOutputs(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)

	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atomA := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","a"]`,
	}
	atomB := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","b"]`,
	}
	require.NoError(t, db.Create(atomA).Error)
	require.NoError(t, db.Create(atomB).Error)

	taskA := &models.Task{
		ID:     uuid.New(),
		JobID:  jobID,
		AtomID: atomA.ID,
		Name:   "step-a",
	}
	taskB := &models.Task{
		ID:     uuid.New(),
		JobID:  jobID,
		AtomID: atomB.ID,
		Name:   "step-b",
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

	require.NoError(t, store.StartTask(runRecord.ID, taskA.ID, "runtime-a"))
	require.NoError(t, store.CompleteTask(runRecord.ID, taskA.ID, "ok", map[string]string{
		"row_count": "42",
	}, nil))

	outputs, err := store.PredecessorOutputs(runRecord.ID, taskB.ID)
	require.NoError(t, err)
	require.Len(t, outputs, 1)
	require.Equal(t, map[string]string{"row_count": "42"}, outputs["step-a"])
}

func TestPredecessorOutputs_NoPredecessors(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)

	taskID := uuid.New()
	runID := uuid.New()

	outputs, err := store.PredecessorOutputs(runID, taskID)
	require.NoError(t, err)
	require.Nil(t, outputs)
}

func TestPredecessorHashes(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)

	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atomA := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","a"]`,
	}
	atomB := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","b"]`,
	}
	require.NoError(t, db.Create(atomA).Error)
	require.NoError(t, db.Create(atomB).Error)

	taskA := &models.Task{
		ID:     uuid.New(),
		JobID:  jobID,
		AtomID: atomA.ID,
		Name:   "step-a",
	}
	taskB := &models.Task{
		ID:     uuid.New(),
		JobID:  jobID,
		AtomID: atomB.ID,
		Name:   "step-b",
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
	require.NoError(t, store.StartTask(runRecord.ID, taskA.ID, "runtime-a"))
	require.NoError(t, store.CompleteTask(runRecord.ID, taskA.ID, "ok", map[string]string{
		"row_count": "42",
	}, nil))
	require.NoError(t, store.SetTaskHash(runRecord.ID, taskA.ID, "pred-hash-1"))

	hashes, err := store.PredecessorHashes(runRecord.ID, taskB.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"pred-hash-1"}, hashes)
}

func TestPredecessorHashesIncludesCachedPredecessors(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)

	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atomA := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","a"]`,
	}
	atomB := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","b"]`,
	}
	require.NoError(t, db.Create(atomA).Error)
	require.NoError(t, db.Create(atomB).Error)

	taskA := &models.Task{
		ID:     uuid.New(),
		JobID:  jobID,
		AtomID: atomA.ID,
		Name:   "step-a",
	}
	taskB := &models.Task{
		ID:     uuid.New(),
		JobID:  jobID,
		AtomID: atomB.ID,
		Name:   "step-b",
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
	require.NoError(t, store.SetTaskHash(runRecord.ID, taskA.ID, "pred-hash-cached"))
	_, err = store.CacheHitTask(runRecord.ID, taskA.ID, CacheHitSource{
		RunID:     uuid.New(),
		CreatedAt: time.Now().UTC(),
	}, "ok", map[string]string{"row_count": "42"}, nil)
	require.NoError(t, err)

	hashes, err := store.PredecessorHashes(runRecord.ID, taskB.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"pred-hash-cached"}, hashes)
}

func TestRegisterTaskSnapshotsResolvedCacheConfig(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})
	t.Setenv("CAESIUM_CACHE_ENABLED", "false")
	t.Setenv("CAESIUM_CACHE_TTL", "30m")

	store := NewStore(db)

	jobCacheJSON, err := json.Marshal(map[string]any{"ttl": "2h"})
	require.NoError(t, err)

	trigger := &models.Trigger{
		ID:    uuid.New(),
		Alias: "cache-trigger",
		Type:  models.TriggerTypeCron,
	}
	require.NoError(t, db.Create(trigger).Error)

	jobID := uuid.New()
	jobModel := &models.Job{
		ID:          jobID,
		Alias:       "cache-job",
		TriggerID:   trigger.ID,
		CacheConfig: datatypes.JSON(jobCacheJSON),
	}
	require.NoError(t, db.Create(jobModel).Error)

	runRecord, err := store.Start(jobID, &trigger.ID)
	require.NoError(t, err)

	atom := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","cache"]`,
	}
	require.NoError(t, db.Create(atom).Error)

	stepCacheJSON, err := json.Marshal(map[string]any{"version": 3})
	require.NoError(t, err)

	task := &models.Task{
		ID:          uuid.New(),
		JobID:       jobID,
		AtomID:      atom.ID,
		Name:        "cacheable",
		CacheConfig: datatypes.JSON(stepCacheJSON),
	}
	require.NoError(t, db.Create(task).Error)

	require.NoError(t, store.RegisterTask(runRecord.ID, task, atom, 0))

	var taskRun models.TaskRun
	require.NoError(t, db.First(&taskRun, "job_run_id = ? AND task_id = ?", runRecord.ID, task.ID).Error)
	require.True(t, taskRun.CacheEnabled)
	require.Equal(t, 2*time.Hour, taskRun.CacheTTL)
	require.Equal(t, 3, taskRun.CacheVersion)
}

func TestCompleteTaskWithBranchSkipLeavesOneSuccessJoinRunnable(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)

	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atom := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","ok"]`,
	}
	require.NoError(t, db.Create(atom).Error)

	decide := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "decide", Type: "branch"}
	fast := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "fast-path"}
	slow := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "slow-path"}
	join := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "join", TriggerRule: "one_success"}
	require.NoError(t, db.Create(decide).Error)
	require.NoError(t, db.Create(fast).Error)
	require.NoError(t, db.Create(slow).Error)
	require.NoError(t, db.Create(join).Error)

	edges := []models.TaskEdge{
		{ID: uuid.New(), JobID: jobID, FromTaskID: decide.ID, ToTaskID: fast.ID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
		{ID: uuid.New(), JobID: jobID, FromTaskID: decide.ID, ToTaskID: slow.ID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
		{ID: uuid.New(), JobID: jobID, FromTaskID: fast.ID, ToTaskID: join.ID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
		{ID: uuid.New(), JobID: jobID, FromTaskID: slow.ID, ToTaskID: join.ID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
	}
	require.NoError(t, db.Create(&edges).Error)

	require.NoError(t, store.RegisterTask(runRecord.ID, decide, atom, 0))
	require.NoError(t, store.RegisterTask(runRecord.ID, fast, atom, 1))
	require.NoError(t, store.RegisterTask(runRecord.ID, slow, atom, 1))
	require.NoError(t, store.RegisterTask(runRecord.ID, join, atom, 2))

	require.NoError(t, store.StartTask(runRecord.ID, decide.ID, "runtime-decide"))
	completeResult, err := store.CompleteTaskWithResult(runRecord.ID, decide.ID, "ok", nil, []string{"fast-path"})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{slow.ID}, completeResult.SkippedTaskIDs)

	state, err := store.Get(runRecord.ID)
	require.NoError(t, err)

	statusByTask := make(map[uuid.UUID]*TaskRun, len(state.Tasks))
	for _, task := range state.Tasks {
		statusByTask[task.TaskID] = task
	}
	require.Equal(t, TaskStatusSkipped, statusByTask[slow.ID].Status)
	require.Equal(t, TaskStatusPending, statusByTask[join.ID].Status)
	require.Equal(t, 1, statusByTask[join.ID].OutstandingPredecessors)

	require.NoError(t, store.StartTask(runRecord.ID, fast.ID, "runtime-fast"))
	completeResult, err = store.CompleteTaskWithResult(runRecord.ID, fast.ID, "ok", nil, nil)
	require.NoError(t, err)
	require.Empty(t, completeResult.SkippedTaskIDs)

	state, err = store.Get(runRecord.ID)
	require.NoError(t, err)

	statusByTask = make(map[uuid.UUID]*TaskRun, len(state.Tasks))
	for _, task := range state.Tasks {
		statusByTask[task.TaskID] = task
	}
	require.Equal(t, TaskStatusPending, statusByTask[join.ID].Status)
	require.Equal(t, 0, statusByTask[join.ID].OutstandingPredecessors)
}

func TestCompleteTaskWithBranchSkipSkipsAllSuccessJoin(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)

	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atom := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","ok"]`,
	}
	require.NoError(t, db.Create(atom).Error)

	decide := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "decide", Type: "branch"}
	fast := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "fast-path"}
	slow := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "slow-path"}
	join := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "join", TriggerRule: "all_success"}
	require.NoError(t, db.Create(decide).Error)
	require.NoError(t, db.Create(fast).Error)
	require.NoError(t, db.Create(slow).Error)
	require.NoError(t, db.Create(join).Error)

	edges := []models.TaskEdge{
		{ID: uuid.New(), JobID: jobID, FromTaskID: decide.ID, ToTaskID: fast.ID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
		{ID: uuid.New(), JobID: jobID, FromTaskID: decide.ID, ToTaskID: slow.ID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
		{ID: uuid.New(), JobID: jobID, FromTaskID: fast.ID, ToTaskID: join.ID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
		{ID: uuid.New(), JobID: jobID, FromTaskID: slow.ID, ToTaskID: join.ID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
	}
	require.NoError(t, db.Create(&edges).Error)

	require.NoError(t, store.RegisterTask(runRecord.ID, decide, atom, 0))
	require.NoError(t, store.RegisterTask(runRecord.ID, fast, atom, 1))
	require.NoError(t, store.RegisterTask(runRecord.ID, slow, atom, 1))
	require.NoError(t, store.RegisterTask(runRecord.ID, join, atom, 2))

	require.NoError(t, store.StartTask(runRecord.ID, decide.ID, "runtime-decide"))
	completeResult, err := store.CompleteTaskWithResult(runRecord.ID, decide.ID, "ok", nil, []string{"fast-path"})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{slow.ID}, completeResult.SkippedTaskIDs)

	require.NoError(t, store.StartTask(runRecord.ID, fast.ID, "runtime-fast"))
	completeResult, err = store.CompleteTaskWithResult(runRecord.ID, fast.ID, "ok", nil, nil)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{join.ID}, completeResult.SkippedTaskIDs)

	state, err := store.Get(runRecord.ID)
	require.NoError(t, err)

	statusByTask := make(map[uuid.UUID]*TaskRun, len(state.Tasks))
	for _, task := range state.Tasks {
		statusByTask[task.TaskID] = task
	}
	require.Equal(t, TaskStatusSkipped, statusByTask[join.ID].Status)
	require.Equal(t, 0, statusByTask[join.ID].OutstandingPredecessors)
}
