package run

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPredecessorGroupSatisfied(t *testing.T) {
	taskID := uuid.New()
	ok := []models.TaskRun{
		{TaskID: taskID, Status: string(TaskStatusSucceeded)},
		{TaskID: taskID, Status: string(TaskStatusCached)},
	}
	assert.True(t, predecessorGroupSatisfied(ok))

	mixed := []models.TaskRun{
		{TaskID: taskID, Status: string(TaskStatusSucceeded)},
		{TaskID: taskID, Status: string(TaskStatusFailed)},
	}
	assert.False(t, predecessorGroupSatisfied(mixed), "one succeeded sibling must not satisfy the group")
}

func TestGroupStatusFromInstances(t *testing.T) {
	taskID := uuid.New()
	assert.Equal(t, TaskStatusSucceeded, groupStatusFromInstances([]models.TaskRun{
		{TaskID: taskID, Status: string(TaskStatusSucceeded)},
		{TaskID: taskID, Status: string(TaskStatusCached)},
	}))
	assert.Equal(t, TaskStatusFailed, groupStatusFromInstances([]models.TaskRun{
		{TaskID: taskID, Status: string(TaskStatusSucceeded)},
		{TaskID: taskID, Status: string(TaskStatusFailed)},
	}))
	assert.Equal(t, TaskStatusSkipped, groupStatusFromInstances([]models.TaskRun{
		{TaskID: taskID, Status: string(TaskStatusSkipped)},
		{TaskID: taskID, Status: string(TaskStatusSkipped)},
	}))
	assert.Equal(t, TaskStatusRunning, groupStatusFromInstances([]models.TaskRun{
		{TaskID: taskID, Status: string(TaskStatusSucceeded)},
		{TaskID: taskID, Status: string(TaskStatusPending)},
	}))
}

func TestAggregatePredecessorStatusesOnePerTask(t *testing.T) {
	pred := uuid.New()
	rows := []models.TaskRun{
		{TaskID: pred, Status: string(TaskStatusSucceeded)},
		{TaskID: pred, Status: string(TaskStatusSucceeded)},
	}
	got := aggregatePredecessorStatuses([]uuid.UUID{pred}, rows)
	require.Len(t, got, 1)
	assert.Equal(t, TaskStatusSucceeded, got[0])
}

// TestExecutionWritesAddressOneFanOutInstance pins the fan-out execution
// identity contract: once a step has been expanded into N instance TaskRun rows
// sharing one (job_run_id, task_id), every execution-side write must address a
// single instance by its TaskRun primary key.
//
// Regression: the local executor called StartTask/SetTaskExitCode/
// SaveTaskLogSnapshot with the catalog task ID. StartTask resolved the row via
// loadUniqueTaskRun, which returns ErrAmbiguousTaskRun for N>1, so the *first*
// instance died immediately after its container started — before the deferred
// engine.Stop that removes the container. The leaked container then made every
// sibling's ContainerCreate fail on the duplicate name, so all N instances
// reported "failed" while the run itself still reached "succeeded".
func TestExecutionWritesAddressOneFanOutInstance(t *testing.T) {
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
		Name:   "process",
	}
	require.NoError(t, db.Create(task).Error)

	// Materialize three fan-out instances for the one catalog task, exactly as
	// the expansion transaction does.
	keys := []string{"a", "b", "c"}
	instanceIDs := make([]uuid.UUID, len(keys))
	for i, key := range keys {
		id := uuid.New()
		instanceIDs[i] = id
		require.NoError(t, db.Create(&models.TaskRun{
			ID:             id,
			JobRunID:       runRecord.ID,
			TaskID:         task.ID,
			AtomID:         atomModel.ID,
			Engine:         models.AtomEngineDocker,
			Image:          atomModel.Image,
			Command:        atomModel.Command,
			Status:         string(TaskStatusPending),
			Attempt:        1,
			MaxAttempts:    1,
			PartitionValue: key,
			PartitionIndex: i,
			PartitionCount: len(keys),
		}).Error)
	}

	// The catalog task ID is genuinely ambiguous now — that is the invariant
	// the executor must stop relying on.
	_, err = loadUniqueTaskRun(db, runRecord.ID, task.ID)
	require.ErrorIs(t, err, ErrAmbiguousTaskRun)

	// Each instance must start independently, addressed by its TaskRun PK.
	for i, id := range instanceIDs {
		require.NoError(t, store.StartTask(runRecord.ID, id, "runtime-"+keys[i]),
			"instance %s must start by TaskRun primary key", keys[i])
	}

	exit := 0
	require.NoError(t, store.SetTaskExitCode(runRecord.ID, instanceIDs[1], &exit))
	require.NoError(t, store.SaveTaskLogSnapshot(runRecord.ID, instanceIDs[1], &TaskLogSnapshot{
		Text: "partition b log",
	}))

	var rows []models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runRecord.ID, task.ID).
		Order("partition_index asc").Find(&rows).Error)
	require.Len(t, rows, 3)

	for i, row := range rows {
		require.Equal(t, string(TaskStatusRunning), row.Status,
			"instance %s should be running", keys[i])
		require.Equal(t, "runtime-"+keys[i], row.RuntimeID,
			"each instance must record its own runtime id, not a sibling's")
		require.NotNil(t, row.StartedAt)
	}

	// Per-instance observability must not bleed across siblings.
	require.Nil(t, rows[0].ExitCode, "exit code must not be written to sibling a")
	require.NotNil(t, rows[1].ExitCode)
	require.Equal(t, 0, *rows[1].ExitCode)
	require.Nil(t, rows[2].ExitCode, "exit code must not be written to sibling c")

	require.Empty(t, rows[0].LogText, "log snapshot must not be written to sibling a")
	require.Equal(t, "partition b log", rows[1].LogText)
	require.Empty(t, rows[2].LogText, "log snapshot must not be written to sibling c")
}

// TestTaskRunsForTaskAndFailureAttribution covers the group-aware lookup the
// read surfaces use in place of an arbitrary `.First()` on
// (job_run_id, task_id). Reading a sibling at random could classify an incident
// from a SUCCEEDED partition's row while the failure lived on another.
func TestTaskRunsForTaskAndFailureAttribution(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atomModel := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo","hi"]`}
	require.NoError(t, db.Create(atomModel).Error)
	task := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atomModel.ID, Name: "process"}
	require.NoError(t, db.Create(task).Error)

	statuses := []TaskStatus{TaskStatusSucceeded, TaskStatusFailed, TaskStatusSucceeded}
	for i, status := range statuses {
		require.NoError(t, db.Create(&models.TaskRun{
			ID:             uuid.New(),
			JobRunID:       runRecord.ID,
			TaskID:         task.ID,
			AtomID:         atomModel.ID,
			Engine:         models.AtomEngineDocker,
			Image:          atomModel.Image,
			Command:        atomModel.Command,
			Status:         string(status),
			Attempt:        1,
			MaxAttempts:    1,
			LogText:        "log-" + string(rune('a'+i)),
			PartitionValue: string(rune('a' + i)),
			PartitionIndex: i,
			PartitionCount: len(statuses),
		}).Error)
	}

	rows, err := store.TaskRunsForTask(runRecord.ID, task.ID)
	require.NoError(t, err)
	require.Len(t, rows, 3, "the whole group is returned, in partition order")
	assert.Equal(t, []string{"a", "b", "c"},
		[]string{rows[0].PartitionValue, rows[1].PartitionValue, rows[2].PartitionValue})

	failed := FailedOrLastTaskRunForTask(rows)
	require.NotNil(t, failed)
	assert.Equal(t, "b", failed.PartitionValue, "the failed instance is the one that explains the failure")
	assert.Equal(t, "log-b", failed.LogText)

	// No rows at all is a nil selection, never a panic.
	assert.Nil(t, FailedOrLastTaskRunForTask(nil))

	// An all-succeeded group falls back to the first instance.
	allGood := []models.TaskRun{
		{PartitionValue: "x", Status: string(TaskStatusSucceeded)},
		{PartitionValue: "y", Status: string(TaskStatusCached)},
	}
	require.NotNil(t, FailedOrLastTaskRunForTask(allGood))
	assert.Equal(t, "x", FailedOrLastTaskRunForTask(allGood).PartitionValue)

	// A non-terminal instance outranks a succeeded one: it is the one still
	// capable of explaining an in-flight problem.
	mixed := []models.TaskRun{
		{PartitionValue: "x", Status: string(TaskStatusSucceeded)},
		{PartitionValue: "y", Status: string(TaskStatusRunning)},
	}
	assert.Equal(t, "y", FailedOrLastTaskRunForTask(mixed).PartitionValue)
}
