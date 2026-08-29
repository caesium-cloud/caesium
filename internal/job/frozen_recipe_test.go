package job

import (
	"context"
	"errors"
	"testing"

	"github.com/caesium-cloud/caesium/internal/atom"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestRetryExecutesRecipeFrozenOnTheRow is the local-lane half of the
// cross-lane contract in issue #354: a re-entered run executes the
// engine/image/command the run was REGISTERED with, not whatever the catalog
// says at re-entry time.
//
// The distributed worker has always done this — it reads taskRun.Engine,
// taskRun.Image and parseTaskCommand(taskRun.Command) off the row
// (internal/worker/runtime_executor.go). The local executor used to rebuild its
// runner from a live svc.Get(t.AtomID), so a `run retry` after a `job apply`
// ran the NEW command locally and the OLD one distributed — the same run,
// two answers, depending on the lane.
//
// The apply is simulated the way the importer performs it: reconcileTasksTx
// MUTATES the existing atom row in place (same atom ID, new engine/image/
// command/spec), so the frozen AtomID still resolves — only its contents moved.
func TestRetryExecutesRecipeFrozenOnTheRow(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskID := uuid.New()
	atomID := uuid.New()

	taskSvc := &fakeTaskService{tasks: models.Tasks{
		{ID: taskID, JobID: jobID, AtomID: atomID},
	}}
	catalogAtom := fakeModelAtom(atomID)
	catalogAtom.Image = "alpine:3.23"
	catalogAtom.Command = `["sh","-c","echo v1"]`
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{atomID: catalogAtom}}
	persistGraph(t, db, taskSvc.tasks, nil)

	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  1,
		TaskFailurePolicy: taskFailurePolicyHalt,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, &fakeTaskEdgeService{}, engine)

	// First run fails, leaving the run in a terminal state a retry can re-open.
	engine.resultByName[taskID.String()] = atom.Failure
	require.Error(t, New(&models.Job{ID: jobID}, opts...).Run(context.Background()))

	snapshot := latestRunSnapshot(t, store, jobID)
	require.Equal(t, run.TaskStatusFailed, taskStatusByID(snapshot)[taskID])

	frozen := taskRunByID(snapshot, taskID)
	require.NotNil(t, frozen)
	require.Equal(t, []string{"sh", "-c", "echo v1"}, frozen.Command)

	// `job apply` lands a new definition for the same step.
	catalogAtom.Image = "example/subject:v2"
	catalogAtom.Command = `["sh","-c","echo v2"]`

	// Retry the run. The reset must leave the recipe columns alone …
	retried, err := store.RetryFromFailure(snapshot.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"sh", "-c", "echo v1"}, taskRunByID(retried, taskID).Command,
		"retryFromFailure must reset scheduling state without rewriting the frozen recipe")

	// … and the re-entered run must execute them.
	delete(engine.resultByName, taskID.String())
	require.NoError(t, New(&models.Job{ID: jobID}, opts...).
		Run(run.WithContext(context.Background(), snapshot.ID)))

	created := engine.createRequestsForTask(taskID)
	require.Len(t, created, 2, "expected one container per attempt")
	require.Equal(t, []string{"sh", "-c", "echo v1"}, created[1].Command,
		"the retry executed the live catalog command, not the one frozen on the task_runs row")
	require.Equal(t, "alpine:3.23", created[1].Image,
		"the retry executed the live catalog image, not the one frozen on the task_runs row")

	final := latestRunSnapshot(t, store, jobID)
	require.Equal(t, run.TaskStatusSucceeded, taskStatusByID(final)[taskID])
}

// TestFirstRunFreezesTheLiveCatalogRecipe is the other half of the contract:
// freezing on re-entry must not mean a run is blind to a definition change. A
// NEW run registers new rows from the current catalog, so applying a change and
// triggering a run picks it up — that is the documented way to adopt a fix.
func TestFirstRunFreezesTheLiveCatalogRecipe(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskID := uuid.New()
	atomID := uuid.New()

	taskSvc := &fakeTaskService{tasks: models.Tasks{
		{ID: taskID, JobID: jobID, AtomID: atomID},
	}}
	catalogAtom := fakeModelAtom(atomID)
	catalogAtom.Command = `["sh","-c","echo v1"]`
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{atomID: catalogAtom}}
	persistGraph(t, db, taskSvc.tasks, nil)

	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  1,
		TaskFailurePolicy: taskFailurePolicyHalt,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, &fakeTaskEdgeService{}, engine)

	require.NoError(t, New(&models.Job{ID: jobID}, opts...).Run(context.Background()))

	catalogAtom.Command = `["sh","-c","echo v2"]`
	require.NoError(t, New(&models.Job{ID: jobID}, opts...).Run(context.Background()))

	created := engine.createRequestsForTask(taskID)
	require.Len(t, created, 2)
	require.Equal(t, []string{"sh", "-c", "echo v1"}, created[0].Command)
	require.Equal(t, []string{"sh", "-c", "echo v2"}, created[1].Command,
		"a NEW run registers fresh rows, so it must pick up the applied definition")
}

// TestRetryUsesTheAttemptBudgetFrozenOnTheRow covers the other execution field
// the row freezes and the distributed worker reads from it: MaxAttempts
// (RegisterTasks writes task.Retries+1; the worker loops to
// taskRun.MaxAttempts). The local lane used to recompute the budget from the
// LIVE task, so a retry after a `job apply` that changed `retries:` gave the
// same run a different number of attempts depending on which lane executed it.
func TestRetryUsesTheAttemptBudgetFrozenOnTheRow(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskID := uuid.New()
	atomID := uuid.New()

	taskModel := &models.Task{ID: taskID, JobID: jobID, AtomID: atomID, Retries: 1}
	taskSvc := &fakeTaskService{tasks: models.Tasks{taskModel}}
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{atomID: fakeModelAtom(atomID)}}
	persistGraph(t, db, taskSvc.tasks, nil)

	// Every attempt fails, on this run and on the retry.
	for _, key := range []string{
		taskID.String(),
		taskID.String() + "-attempt2",
		taskID.String() + "-attempt3",
		taskID.String() + "-attempt4",
	} {
		engine.createErrByName[key] = errors.New("always fails")
	}

	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  1,
		TaskFailurePolicy: taskFailurePolicyHalt,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, &fakeTaskEdgeService{}, engine)

	// retries: 1 -> two attempts, and the row freezes max_attempts = 2.
	require.Error(t, New(&models.Job{ID: jobID}, opts...).Run(context.Background()))
	require.Len(t, engine.createRequestsForTask(taskID), 2)

	snapshot := latestRunSnapshot(t, store, jobID)
	require.Equal(t, 2, taskRunByID(snapshot, taskID).MaxAttempts)

	// `job apply` raises the step's retry budget.
	taskModel.Retries = 3

	_, err := store.RetryFromFailure(snapshot.ID)
	require.NoError(t, err)
	require.Error(t, New(&models.Job{ID: jobID}, opts...).
		Run(run.WithContext(context.Background(), snapshot.ID)))

	require.Len(t, engine.createRequestsForTask(taskID), 4,
		"the retry ran the applied retry budget instead of the 2 attempts the row froze")
}
