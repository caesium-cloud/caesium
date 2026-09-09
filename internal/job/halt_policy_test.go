package job

import (
	"context"
	"errors"
	"testing"
	"time"

	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Under CAESIUM_TASK_FAILURE_POLICY=halt (the default) a failure stops the run
// from admitting new work — but not the steps whose trigger rule says they run
// regardless of upstream failure, and not the work that had already started.
// The Kahn loop used to clear its whole queue on the first failure, so an
// `all_done` cleanup downstream of the failure was never dispatched on this
// lane even though the schema promises it runs "no matter what" (#401).
//
//	fail ──┬──▶ cleanup (all_done)
//	       └──▶ strict  (all_success)
//	slow ──▶ independent (all_success) ──▶ late-cleanup (all_done)
//
// `fail` and `slow` are dispatched together (MaxParallelTasks: 2); `fail`
// fails immediately, while `slow` is still running. Expected: `cleanup` runs,
// `strict` is rule-skipped, `slow` finishes on its own, `independent` is
// HALTED — it is the new independent work halt exists to stop — and
// `late-cleanup` still runs because `all_done` is satisfied by a skipped
// predecessor. The run ends failed on `fail`'s error, whatever the cleanups do.
func TestRunLocalHaltPolicyRunsTolerantSuccessorsOnly(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskFail := uuid.New()
	taskSlow := uuid.New()
	taskCleanup := uuid.New()
	taskStrict := uuid.New()
	taskIndependent := uuid.New()
	taskLateCleanup := uuid.New()

	taskSvc := &fakeTaskService{tasks: models.Tasks{
		{ID: taskFail, JobID: jobID, AtomID: uuid.New(), Name: "fail", Position: 0, TriggerRule: jobdefschema.TriggerRuleAllSuccess},
		{ID: taskSlow, JobID: jobID, AtomID: uuid.New(), Name: "slow", Position: 1, TriggerRule: jobdefschema.TriggerRuleAllSuccess},
		{ID: taskCleanup, JobID: jobID, AtomID: uuid.New(), Name: "cleanup", Position: 2, TriggerRule: jobdefschema.TriggerRuleAllDone},
		{ID: taskStrict, JobID: jobID, AtomID: uuid.New(), Name: "strict", Position: 3, TriggerRule: jobdefschema.TriggerRuleAllSuccess},
		{ID: taskIndependent, JobID: jobID, AtomID: uuid.New(), Name: "independent", Position: 4, TriggerRule: jobdefschema.TriggerRuleAllSuccess},
		{ID: taskLateCleanup, JobID: jobID, AtomID: uuid.New(), Name: "late-cleanup", Position: 5, TriggerRule: jobdefschema.TriggerRuleAllDone},
	}}
	atoms := map[uuid.UUID]*models.Atom{}
	for _, task := range taskSvc.tasks {
		atoms[task.AtomID] = fakeModelAtom(task.AtomID)
	}
	atomSvc := &fakeAtomService{atoms: atoms}
	edgeSvc := &fakeTaskEdgeService{edges: models.TaskEdges{
		{ID: uuid.New(), JobID: jobID, FromTaskID: taskFail, ToTaskID: taskCleanup},
		{ID: uuid.New(), JobID: jobID, FromTaskID: taskFail, ToTaskID: taskStrict},
		{ID: uuid.New(), JobID: jobID, FromTaskID: taskSlow, ToTaskID: taskIndependent},
		{ID: uuid.New(), JobID: jobID, FromTaskID: taskIndependent, ToTaskID: taskLateCleanup},
	}}
	persistGraph(t, db, taskSvc.tasks, edgeSvc.edges)

	engine.createErrByName[taskFail.String()] = errors.New("fail exploded")
	engine.runDurationByName[taskSlow.String()] = 150 * time.Millisecond

	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  2,
		TaskFailurePolicy: taskFailurePolicyHalt,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, edgeSvc, engine)

	err := New(&models.Job{ID: jobID}, opts...).Run(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "fail exploded", "the run fails on the ORIGINAL failure, not on what the cleanups did")

	snapshot := latestRunSnapshot(t, store, jobID)
	require.Equal(t, run.StatusFailed, snapshot.Status, "a halted run with a successful cleanup still ends failed")
	status := taskStatusByID(snapshot)
	require.Equal(t, run.TaskStatusFailed, status[taskFail])
	require.Equal(t, run.TaskStatusSucceeded, status[taskCleanup], "all_done downstream of the failure must run under halt")
	require.Equal(t, run.TaskStatusSkipped, status[taskStrict])
	require.Equal(t, `trigger rule "all_success" not satisfied`, taskRunByID(snapshot, taskStrict).Error)
	require.Equal(t, run.TaskStatusSucceeded, status[taskSlow], "work that had started finishes on its own")
	require.Equal(t, run.TaskStatusSkipped, status[taskIndependent], "independent work not yet started is halted")
	require.Equal(t, run.HaltReason("fail"), taskRunByID(snapshot, taskIndependent).Error)
	require.Empty(t, engine.createRequestsForTask(taskIndependent), "a halted step must never reach the engine")
	require.Equal(t, run.TaskStatusSucceeded, status[taskLateCleanup], "all_done downstream of a halted branch runs: its predecessor resolved (skipped)")
	require.Len(t, engine.createRequestsForTask(taskLateCleanup), 1)

	// No row is left pending on a terminal run.
	for _, taskState := range snapshot.Tasks {
		require.True(t, run.IsTerminal(taskState.Status), "task %s left %s on a terminal run", taskState.ID, taskState.Status)
	}
}

// A tolerant step that itself fails after the halt is handled the same way:
// the run stays failed, its own tolerant successors still run, and nothing
// intolerant is admitted.
func TestRunLocalHaltPolicyFailingCleanupKeepsTolerantChainRunning(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskFail := uuid.New()
	taskCleanup := uuid.New()
	taskNotify := uuid.New()
	taskPublish := uuid.New()

	taskSvc := &fakeTaskService{tasks: models.Tasks{
		{ID: taskFail, JobID: jobID, AtomID: uuid.New(), Name: "fail", Position: 0},
		{ID: taskCleanup, JobID: jobID, AtomID: uuid.New(), Name: "cleanup", Position: 1, TriggerRule: jobdefschema.TriggerRuleAllDone},
		{ID: taskNotify, JobID: jobID, AtomID: uuid.New(), Name: "notify", Position: 2, TriggerRule: jobdefschema.TriggerRuleAlways},
		{ID: taskPublish, JobID: jobID, AtomID: uuid.New(), Name: "publish", Position: 3, TriggerRule: jobdefschema.TriggerRuleAllSuccess},
	}}
	atoms := map[uuid.UUID]*models.Atom{}
	for _, task := range taskSvc.tasks {
		atoms[task.AtomID] = fakeModelAtom(task.AtomID)
	}
	atomSvc := &fakeAtomService{atoms: atoms}
	edgeSvc := &fakeTaskEdgeService{edges: models.TaskEdges{
		{ID: uuid.New(), JobID: jobID, FromTaskID: taskFail, ToTaskID: taskCleanup},
		{ID: uuid.New(), JobID: jobID, FromTaskID: taskCleanup, ToTaskID: taskNotify},
		{ID: uuid.New(), JobID: jobID, FromTaskID: taskCleanup, ToTaskID: taskPublish},
	}}
	persistGraph(t, db, taskSvc.tasks, edgeSvc.edges)

	engine.createErrByName[taskFail.String()] = errors.New("fail exploded")
	engine.createErrByName[taskCleanup.String()] = errors.New("cleanup exploded")

	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  1,
		TaskFailurePolicy: taskFailurePolicyHalt,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, edgeSvc, engine)

	err := New(&models.Job{ID: jobID}, opts...).Run(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "fail exploded")

	snapshot := latestRunSnapshot(t, store, jobID)
	require.Equal(t, run.StatusFailed, snapshot.Status)
	status := taskStatusByID(snapshot)
	require.Equal(t, run.TaskStatusFailed, status[taskFail])
	require.Equal(t, run.TaskStatusFailed, status[taskCleanup], "the released cleanup ran (and failed)")
	require.Equal(t, run.TaskStatusSucceeded, status[taskNotify], "`always` downstream of the failed cleanup still runs")
	require.Equal(t, run.TaskStatusSkipped, status[taskPublish])
	require.Equal(t, run.HaltReason("fail"), taskRunByID(snapshot, taskPublish).Error,
		"an all_success step downstream of a tolerant one is still new work the halt refuses")
	require.Empty(t, engine.createRequestsForTask(taskPublish))
}

// waitHaltFixture seeds the distributed waiter's view of the DAG directly in
// the store — the waiter never dispatches, it only reads rows and sweeps — so
// the test can play the worker.
//
//	fail ──▶ cleanup (all_done)
//	independent (root, all_success)
func waitHaltFixture(t *testing.T) (*run.Store, uuid.UUID, map[string]*models.Task) {
	t.Helper()
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
	store := run.NewStore(db)

	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "wait-halt"}).Error)
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)
	atomModel := fakeModelAtom(uuid.New())
	require.NoError(t, db.Create(atomModel).Error)

	tasks := map[string]*models.Task{
		"fail":        {ID: uuid.New(), JobID: jobID, AtomID: atomModel.ID, Name: "fail", Position: 0, Type: "task", TriggerRule: jobdefschema.TriggerRuleAllSuccess},
		"cleanup":     {ID: uuid.New(), JobID: jobID, AtomID: atomModel.ID, Name: "cleanup", Position: 1, Type: "task", TriggerRule: jobdefschema.TriggerRuleAllDone},
		"independent": {ID: uuid.New(), JobID: jobID, AtomID: atomModel.ID, Name: "independent", Position: 2, Type: "task", TriggerRule: jobdefschema.TriggerRuleAllSuccess},
	}
	for _, name := range []string{"fail", "cleanup", "independent"} {
		require.NoError(t, db.Create(tasks[name]).Error)
	}
	require.NoError(t, db.Create(&models.TaskEdge{ID: uuid.New(), JobID: jobID, FromTaskID: tasks["fail"].ID, ToTaskID: tasks["cleanup"].ID}).Error)
	require.NoError(t, store.RegisterTasks(runRecord.ID, []run.RegisterTaskInput{
		{Task: tasks["fail"], Atom: atomModel, OutstandingPredecessors: 0},
		{Task: tasks["cleanup"], Atom: atomModel, OutstandingPredecessors: 1},
		{Task: tasks["independent"], Atom: atomModel, OutstandingPredecessors: 0},
	}))
	require.NoError(t, store.StartTask(runRecord.ID, tasks["fail"].ID, "fail-container"))
	require.NoError(t, store.FailTask(runRecord.ID, tasks["fail"].ID, errors.New("boom")))
	return store, runRecord.ID, tasks
}

func waitHaltRow(t *testing.T, store *run.Store, runID, taskID uuid.UUID) *run.TaskRun {
	t.Helper()
	snapshot, err := store.Get(runID)
	require.NoError(t, err)
	row := taskRunByID(snapshot, taskID)
	require.NotNil(t, row)
	return row
}

// The distributed waiter used to finalize a run the moment a task had failed
// and nothing was running — which is exactly the instant a failed
// predecessor's released `all_done` successor is waiting to be claimed. It now
// halts the independent work itself and waits for the store to say the run is
// complete.
func TestWaitForRunCompletionHaltWaitsForReleasedTolerantSuccessor(t *testing.T) {
	store, runID, tasks := waitHaltFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- waitForRunCompletion(ctx, store, runID, len(tasks), false, 10*time.Millisecond) }()

	// The waiter must (a) halt the independent root and (b) keep waiting while
	// the released cleanup is dispatchable.
	require.Eventually(t, func() bool {
		return waitHaltRow(t, store, runID, tasks["independent"].ID).Status == run.TaskStatusSkipped
	}, 2*time.Second, 10*time.Millisecond, "the waiter's own sweep must halt the independent root")
	require.Equal(t, run.HaltReason("fail"), waitHaltRow(t, store, runID, tasks["independent"].ID).Error)
	select {
	case err := <-done:
		t.Fatalf("waiter finalized the run while its released all_done successor was still pending: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	cleanup := waitHaltRow(t, store, runID, tasks["cleanup"].ID)
	require.Equal(t, run.TaskStatusPending, cleanup.Status)
	require.Zero(t, cleanup.OutstandingPredecessors)

	// Play the worker: the cleanup runs and succeeds. The run is now complete
	// — and failed, on the upstream failure.
	require.NoError(t, store.StartTask(runID, tasks["cleanup"].ID, "cleanup-container"))
	require.NoError(t, store.CompleteTask(runID, tasks["cleanup"].ID, "success", nil, nil))
	select {
	case err := <-done:
		require.Error(t, err)
		require.Contains(t, err.Error(), "completed with 1 failed task(s)")
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not return once every row was terminal")
	}
}

// Under `continue` the waiter halts nothing: the independent root stays
// dispatchable and the run completes only when every row is terminal.
func TestWaitForRunCompletionContinueLeavesIndependentWorkAlone(t *testing.T) {
	store, runID, tasks := waitHaltFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- waitForRunCompletion(ctx, store, runID, len(tasks), true, 10*time.Millisecond) }()

	select {
	case err := <-done:
		t.Fatalf("waiter finalized a run with dispatchable work: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	require.Equal(t, run.TaskStatusPending, waitHaltRow(t, store, runID, tasks["independent"].ID).Status)

	for _, name := range []string{"cleanup", "independent"} {
		require.NoError(t, store.StartTask(runID, tasks[name].ID, name+"-container"))
		require.NoError(t, store.CompleteTask(runID, tasks[name].ID, "success", nil, nil))
	}
	select {
	case err := <-done:
		require.Error(t, err)
		require.Contains(t, err.Error(), "completed with 1 failed task(s)")
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not return once every row was terminal")
	}
}
