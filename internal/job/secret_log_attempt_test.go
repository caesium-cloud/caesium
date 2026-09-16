package job

import (
	"errors"
	"strings"
	"testing"

	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestSecretLogResumeUsesPersistedUnfannedAttempt(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
	store := run.NewStore(db)
	engine := newFakeEngine()
	jobID, taskID, atomID := uuid.New(), uuid.New(), uuid.New()
	tasks := models.Tasks{{ID: taskID, JobID: jobID, AtomID: atomID, Name: "secret"}}
	taskSvc := &fakeTaskService{tasks: tasks}
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{
		atomID: fakeModelAtomWithEnv(t, atomID, map[string]string{"TOKEN": "secret://env/TOKEN"}),
	}}
	persistGraph(t, db, tasks, nil)
	engine.createErrByName[taskID.String()] = errors.New("first attempt interrupted")
	opts := append(withTestDeps(store, env.Environment{
		MaxParallelTasks: 1, TaskFailurePolicy: taskFailurePolicyHalt, ExecutionMode: executionModeLocal,
	}, taskSvc, atomSvc, &fakeTaskEdgeService{}, engine),
		WithSecretResolver(localStaticSecretResolver{"secret://env/TOKEN": "resolved-token"}))

	require.Error(t, New(&models.Job{ID: jobID}, opts...).Run(t.Context()))
	failed := latestRunSnapshot(t, store, jobID)
	require.NoError(t, db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", failed.ID, taskID).
		Update("max_attempts", 2).Error)
	_, err := store.RetryFromFailure(failed.ID)
	require.NoError(t, err)
	require.NoError(t, db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", failed.ID, taskID).
		Update("attempt", 2).Error)
	delete(engine.createErrByName, taskID.String())

	require.NoError(t, New(&models.Job{ID: jobID}, opts...).Run(run.WithContext(t.Context(), failed.ID)))
	resumed := latestRunSnapshot(t, store, jobID)
	taskRun := taskRunByID(resumed, taskID)
	require.NotNil(t, taskRun)
	require.Equal(t, 2, taskRun.Attempt)
	require.Equal(t, run.TaskStatusSucceeded, taskRun.Status)
	require.True(t, taskRun.LogScrubbed)
	require.NotEmpty(t, taskRun.LogGeneration)
	requests := engine.createRequestsForTask(taskID)
	require.Len(t, requests, 2)
	require.True(t, strings.HasSuffix(requests[1].Name, "-attempt2"), requests[1].Name)
}

func TestSecretLogResumeUsesPersistedFanOutAttempt(t *testing.T) {
	f := newFanOutFixture(t, `["retry"]`, &schema.FanOut{
		From: "list", MaxPartitions: 4, FailurePolicy: schema.FanOutFailureContinue,
	}, 0)
	atomID := f.taskSvc.tasks[1].AtomID
	f.atomSvc.atoms[atomID] = fakeModelAtomWithEnv(t, atomID, map[string]string{
		"TOKEN": "secret://env/TOKEN",
	})
	f.engine.createErrByPartition["retry"] = errors.New("first attempt interrupted")
	opts := append(withTestDeps(f.store, defaultFanOutVars(), f.taskSvc, f.atomSvc, f.edgeSvc, f.engine),
		WithSecretResolver(localStaticSecretResolver{"secret://env/TOKEN": "resolved-token"}))

	require.Error(t, New(&models.Job{ID: f.jobID}, opts...).Run(t.Context()))
	jobRun := f.latestJobRun(t)
	instances := f.instanceRowsFor(t, jobRun.ID)
	require.Len(t, instances, 1)
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", instances[0].ID).
		Update("max_attempts", 2).Error)
	_, _, err := f.store.RetryPartition(t.Context(), jobRun.ID, instances[0].ID)
	require.NoError(t, err)
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", instances[0].ID).
		Update("attempt", 2).Error)
	delete(f.engine.createErrByPartition, "retry")

	require.NoError(t, New(&models.Job{ID: f.jobID}, opts...).Run(run.WithContext(t.Context(), jobRun.ID)))
	instances = f.instanceRowsFor(t, jobRun.ID)
	require.Len(t, instances, 1)
	require.Equal(t, 2, instances[0].Attempt)
	require.Equal(t, string(run.TaskStatusSucceeded), instances[0].Status)
	require.True(t, instances[0].LogScrubbed)
	require.NotEmpty(t, instances[0].LogGeneration)
	requests := f.engine.createRequestsForTask(f.fanned)
	require.Len(t, requests, 2)
	require.True(t, strings.HasSuffix(requests[1].Name, "-attempt2"), requests[1].Name)
}
