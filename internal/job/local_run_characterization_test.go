package job

import (
	"context"
	"errors"
	"testing"

	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These exercise New.Run and its completion defer. Existing local fan-out,
// cache, branch, drain, cancellation and deadline tests cover their execution
// paths; these pin the additional error handoff that extraction must preserve.
func characterizationJob(t *testing.T, count int, edgeIndices [][2]int) (*job, *run.Store, *fakeEngine, models.Tasks) {
	t.Helper()
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
	store := run.NewStore(db)
	engine := newFakeEngine()
	jobID := uuid.New()
	tasks := make(models.Tasks, count)
	atoms := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{}}
	for i := range tasks {
		tasks[i] = &models.Task{ID: uuid.New(), JobID: jobID, AtomID: uuid.New(), Position: i}
		atoms.atoms[tasks[i].AtomID] = fakeModelAtom(tasks[i].AtomID)
	}
	edges := make(models.TaskEdges, 0, len(edgeIndices))
	for _, pair := range edgeIndices {
		edges = append(edges, &models.TaskEdge{ID: uuid.New(), JobID: jobID, FromTaskID: tasks[pair[0]].ID, ToTaskID: tasks[pair[1]].ID})
	}
	persistGraph(t, db, tasks, edges)
	opts := withTestDeps(store, env.Environment{ExecutionMode: executionModeLocal, MaxParallelTasks: 1},
		&fakeTaskService{tasks: tasks}, atoms, &fakeTaskEdgeService{edges: edges}, engine)
	return New(&models.Job{ID: jobID}, opts...).(*job), store, engine, tasks
}

func TestLocalRunOrdinaryErrorCompletionPrecedesCallback(t *testing.T) {
	j, store, engine, tasks := characterizationJob(t, 1, nil)
	ordinary := errors.New("ordinary creation failure")
	engine.createErrByName[tasks[0].ID.String()] = ordinary
	var order []string
	j.beforeComplete = func(runID uuid.UUID) {
		snapshot, err := store.Get(runID)
		require.NoError(t, err)
		require.Equal(t, run.StatusRunning, snapshot.Status)
		order = append(order, "before completion")
	}
	var callbackErr error
	j.dispatchRunCallbacks = func(ctx context.Context, jobID, runID uuid.UUID, err error) error {
		require.NoError(t, ctx.Err())
		snapshot, readErr := store.Get(runID)
		require.NoError(t, readErr)
		require.Equal(t, run.StatusFailed, snapshot.Status)
		require.NotNil(t, snapshot.CompletedAt)
		callbackErr = err
		order = append(order, "callback")
		return errors.New("callback failure is only logged")
	}
	returnedErr := j.Run(context.Background())
	require.ErrorIs(t, returnedErr, ordinary)
	require.Same(t, returnedErr, callbackErr)
	require.Equal(t, []string{"before completion", "callback"}, order)
	require.Equal(t, run.TaskStatusFailed, taskRunByID(latestRunSnapshot(t, store, j.id), tasks[0].ID).Status)
}

// TODO: known pre-existing bug: unresolved pending work is currently persisted
// as succeeded and emits success callbacks. This characterization documents the
// defect for a separate fix; it is not an intended execution contract.
func TestLocalRunKnownIssueUnresolvedDependenciesCompleteAsSucceeded(t *testing.T) {
	// A runnable root completes while a disconnected cycle stays unresolved.
	j, store, engine, tasks := characterizationJob(t, 3, [][2]int{{1, 2}, {2, 1}})
	callbacks := 0
	j.dispatchRunCallbacks = func(_ context.Context, _, runID uuid.UUID, completionErr error) error {
		callbacks++
		require.NoError(t, completionErr, "the unresolved final return does not assign runErr")
		snapshot, err := store.Get(runID)
		require.NoError(t, err)
		require.Equal(t, run.StatusSucceeded, snapshot.Status, "completion uses its separate nil error")
		return nil
	}
	returnedErr := j.Run(context.Background())
	require.ErrorContains(t, returnedErr, "remaining tasks may be waiting on unresolved dependencies")
	require.Equal(t, 1, callbacks)
	snapshot := latestRunSnapshot(t, store, j.id)
	require.Equal(t, run.TaskStatusSucceeded, taskRunByID(snapshot, tasks[0].ID).Status)
	for _, task := range tasks[1:] {
		require.Equal(t, run.TaskStatusPending, taskRunByID(snapshot, task.ID).Status)
		require.Empty(t, engine.createRequestsForTask(task.ID))
	}
}

func TestLocalRunPreservedFailureSeedsCompletionError(t *testing.T) {
	j, store, engine, tasks := characterizationJob(t, 2, [][2]int{{0, 1}})
	snapshot, err := store.Start(j.id, nil)
	require.NoError(t, err)
	inputs := make([]run.RegisterTaskInput, 0, len(tasks))
	for i, task := range tasks {
		inputs = append(inputs, run.RegisterTaskInput{Task: task, Atom: fakeModelAtom(task.AtomID), OutstandingPredecessors: i})
	}
	require.NoError(t, store.RegisterTasks(snapshot.ID, inputs))
	require.NoError(t, store.FailTask(snapshot.ID, tasks[1].ID, errors.New("settled failure")))
	var callbackErr error
	j.dispatchRunCallbacks = func(_ context.Context, _, runID uuid.UUID, completionErr error) error {
		callbackErr = completionErr
		completed, readErr := store.Get(runID)
		require.NoError(t, readErr)
		require.Equal(t, run.StatusFailed, completed.Status)
		return nil
	}
	returnedErr := j.Run(run.WithContext(context.Background(), snapshot.ID))
	require.ErrorContains(t, returnedErr, "previously failed")
	require.Same(t, returnedErr, callbackErr)
	require.Len(t, engine.createRequestsForTask(tasks[0].ID), 1)
	require.Empty(t, engine.createRequestsForTask(tasks[1].ID))
	completed, err := store.Get(snapshot.ID)
	require.NoError(t, err)
	require.Equal(t, run.TaskStatusSucceeded, taskRunByID(completed, tasks[0].ID).Status)
	require.Equal(t, "settled failure", taskRunByID(completed, tasks[1].ID).Error)
}
