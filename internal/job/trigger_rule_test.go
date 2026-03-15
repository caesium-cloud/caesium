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

// ─── satisfiesTriggerRule unit tests ────────────────────────────────────────

func TestSatisfiesTriggerRule(t *testing.T) {
	succ := run.TaskStatusSucceeded
	fail := run.TaskStatusFailed
	skip := run.TaskStatusSkipped

	tests := []struct {
		name     string
		rule     string
		statuses []run.TaskStatus
		wantRun  bool
	}{
		// all_success
		{name: "all_success/all_succeeded", rule: jobdefschema.TriggerRuleAllSuccess, statuses: []run.TaskStatus{succ, succ}, wantRun: true},
		{name: "all_success/one_failed", rule: jobdefschema.TriggerRuleAllSuccess, statuses: []run.TaskStatus{succ, fail}, wantRun: false},
		{name: "all_success/all_failed", rule: jobdefschema.TriggerRuleAllSuccess, statuses: []run.TaskStatus{fail, fail}, wantRun: false},
		{name: "all_success/one_skipped", rule: jobdefschema.TriggerRuleAllSuccess, statuses: []run.TaskStatus{succ, skip}, wantRun: false},
		{name: "all_success/no_preds", rule: jobdefschema.TriggerRuleAllSuccess, statuses: nil, wantRun: true},

		// default (empty rule = all_success)
		{name: "default/all_succeeded", rule: "", statuses: []run.TaskStatus{succ}, wantRun: true},
		{name: "default/one_failed", rule: "", statuses: []run.TaskStatus{fail}, wantRun: false},

		// all_done
		{name: "all_done/all_succeeded", rule: jobdefschema.TriggerRuleAllDone, statuses: []run.TaskStatus{succ, succ}, wantRun: true},
		{name: "all_done/all_failed", rule: jobdefschema.TriggerRuleAllDone, statuses: []run.TaskStatus{fail, fail}, wantRun: true},
		{name: "all_done/mixed", rule: jobdefschema.TriggerRuleAllDone, statuses: []run.TaskStatus{succ, fail, skip}, wantRun: true},
		{name: "all_done/no_preds", rule: jobdefschema.TriggerRuleAllDone, statuses: nil, wantRun: true},

		// all_failed
		{name: "all_failed/all_failed", rule: jobdefschema.TriggerRuleAllFailed, statuses: []run.TaskStatus{fail, fail}, wantRun: true},
		{name: "all_failed/one_succeeded", rule: jobdefschema.TriggerRuleAllFailed, statuses: []run.TaskStatus{succ, fail}, wantRun: false},
		{name: "all_failed/all_succeeded", rule: jobdefschema.TriggerRuleAllFailed, statuses: []run.TaskStatus{succ}, wantRun: false},
		{name: "all_failed/no_preds", rule: jobdefschema.TriggerRuleAllFailed, statuses: nil, wantRun: true},

		// one_success
		{name: "one_success/one_succeeded", rule: jobdefschema.TriggerRuleOneSuccess, statuses: []run.TaskStatus{succ, fail}, wantRun: true},
		{name: "one_success/all_failed", rule: jobdefschema.TriggerRuleOneSuccess, statuses: []run.TaskStatus{fail, fail}, wantRun: false},
		{name: "one_success/all_succeeded", rule: jobdefschema.TriggerRuleOneSuccess, statuses: []run.TaskStatus{succ, succ}, wantRun: true},
		{name: "one_success/no_preds", rule: jobdefschema.TriggerRuleOneSuccess, statuses: nil, wantRun: true},

		// always
		{name: "always/all_succeeded", rule: jobdefschema.TriggerRuleAlways, statuses: []run.TaskStatus{succ}, wantRun: true},
		{name: "always/all_failed", rule: jobdefschema.TriggerRuleAlways, statuses: []run.TaskStatus{fail}, wantRun: true},
		{name: "always/mixed", rule: jobdefschema.TriggerRuleAlways, statuses: []run.TaskStatus{succ, fail, skip}, wantRun: true},
		{name: "always/no_preds", rule: jobdefschema.TriggerRuleAlways, statuses: nil, wantRun: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := satisfiesTriggerRule(tt.rule, tt.statuses)
			require.Equal(t, tt.wantRun, got)
		})
	}
}

// ─── isTolerantRule unit tests ───────────────────────────────────────────────

func TestIsTolerantRule(t *testing.T) {
	tests := []struct {
		rule    string
		wantTol bool
	}{
		{jobdefschema.TriggerRuleAllSuccess, false},
		{jobdefschema.TriggerRuleOneSuccess, true},
		{jobdefschema.TriggerRuleAllDone, true},
		{jobdefschema.TriggerRuleAllFailed, true},
		{jobdefschema.TriggerRuleAlways, true},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.rule, func(t *testing.T) {
			require.Equal(t, tt.wantTol, isTolerantRule(tt.rule))
		})
	}
}

// ─── integration tests ───────────────────────────────────────────────────────

// TestAllDoneTaskRunsAfterUpstreamFailure verifies that a task with
// triggerRule=all_done executes even when its predecessor fails, and that a
// sibling task with no tolerant rule is skipped.
func TestAllDoneTaskRunsAfterUpstreamFailure(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskUpstream := uuid.New()
	taskCleanup := uuid.New() // all_done — should run even if upstream fails
	taskNormal := uuid.New()  // all_success (default) — should be skipped

	taskSvc := &fakeTaskService{tasks: models.Tasks{
		{ID: taskUpstream, JobID: jobID, AtomID: uuid.New(), TriggerRule: jobdefschema.TriggerRuleAllSuccess},
		{ID: taskCleanup, JobID: jobID, AtomID: uuid.New(), TriggerRule: jobdefschema.TriggerRuleAllDone},
		{ID: taskNormal, JobID: jobID, AtomID: uuid.New(), TriggerRule: jobdefschema.TriggerRuleAllSuccess},
	}}
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{
		taskSvc.tasks[0].AtomID: fakeModelAtom(taskSvc.tasks[0].AtomID),
		taskSvc.tasks[1].AtomID: fakeModelAtom(taskSvc.tasks[1].AtomID),
		taskSvc.tasks[2].AtomID: fakeModelAtom(taskSvc.tasks[2].AtomID),
	}}
	edgeSvc := &fakeTaskEdgeService{edges: models.TaskEdges{
		{ID: uuid.New(), JobID: jobID, FromTaskID: taskUpstream, ToTaskID: taskCleanup},
		{ID: uuid.New(), JobID: jobID, FromTaskID: taskUpstream, ToTaskID: taskNormal},
	}}
	persistGraph(t, db, taskSvc.tasks, edgeSvc.edges)

	// Make the upstream task fail.
	engine.createErrByName[taskUpstream.String()] = errors.New("upstream failed")

	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  1,
		TaskFailurePolicy: taskFailurePolicyContinue,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, edgeSvc, engine)

	err := New(&models.Job{ID: jobID}, opts...).Run(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "upstream failed")

	snapshot := latestRunSnapshot(t, store, jobID)
	status := taskStatusByID(snapshot)
	require.Equal(t, run.TaskStatusFailed, status[taskUpstream], "upstream should be failed")
	require.Equal(t, run.TaskStatusSucceeded, status[taskCleanup], "all_done task should have run and succeeded")
	require.Equal(t, run.TaskStatusSkipped, status[taskNormal], "all_success task should be skipped")
}

// TestAllFailedTaskRunsOnlyWhenAllFail verifies that a task with
// triggerRule=all_failed only runs when all predecessors failed.
func TestAllFailedTaskRunsOnlyWhenAllFail(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskUpstream := uuid.New()
	taskOnFailure := uuid.New() // all_failed — should only run if upstream failed

	taskSvc := &fakeTaskService{tasks: models.Tasks{
		{ID: taskUpstream, JobID: jobID, AtomID: uuid.New(), TriggerRule: jobdefschema.TriggerRuleAllSuccess},
		{ID: taskOnFailure, JobID: jobID, AtomID: uuid.New(), TriggerRule: jobdefschema.TriggerRuleAllFailed},
	}}
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{
		taskSvc.tasks[0].AtomID: fakeModelAtom(taskSvc.tasks[0].AtomID),
		taskSvc.tasks[1].AtomID: fakeModelAtom(taskSvc.tasks[1].AtomID),
	}}
	edgeSvc := &fakeTaskEdgeService{edges: models.TaskEdges{
		{ID: uuid.New(), JobID: jobID, FromTaskID: taskUpstream, ToTaskID: taskOnFailure},
	}}
	persistGraph(t, db, taskSvc.tasks, edgeSvc.edges)

	engine.createErrByName[taskUpstream.String()] = errors.New("upstream failed")

	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  1,
		TaskFailurePolicy: taskFailurePolicyContinue,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, edgeSvc, engine)

	err := New(&models.Job{ID: jobID}, opts...).Run(context.Background())
	require.Error(t, err)

	snapshot := latestRunSnapshot(t, store, jobID)
	status := taskStatusByID(snapshot)
	require.Equal(t, run.TaskStatusFailed, status[taskUpstream])
	require.Equal(t, run.TaskStatusSucceeded, status[taskOnFailure], "all_failed task should run when upstream failed")
}

// TestAllFailedSkippedWhenUpstreamSucceeds verifies that a task with
// triggerRule=all_failed is skipped when the upstream succeeded.
func TestAllFailedSkippedWhenUpstreamSucceeds(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskUpstream := uuid.New()
	taskOnFailure := uuid.New()

	taskSvc := &fakeTaskService{tasks: models.Tasks{
		{ID: taskUpstream, JobID: jobID, AtomID: uuid.New(), TriggerRule: jobdefschema.TriggerRuleAllSuccess},
		{ID: taskOnFailure, JobID: jobID, AtomID: uuid.New(), TriggerRule: jobdefschema.TriggerRuleAllFailed},
	}}
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{
		taskSvc.tasks[0].AtomID: fakeModelAtom(taskSvc.tasks[0].AtomID),
		taskSvc.tasks[1].AtomID: fakeModelAtom(taskSvc.tasks[1].AtomID),
	}}
	edgeSvc := &fakeTaskEdgeService{edges: models.TaskEdges{
		{ID: uuid.New(), JobID: jobID, FromTaskID: taskUpstream, ToTaskID: taskOnFailure},
	}}
	persistGraph(t, db, taskSvc.tasks, edgeSvc.edges)

	// upstream succeeds — all_failed task should be skipped
	engine.runDurationByName[taskUpstream.String()] = 10 * time.Millisecond

	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  1,
		TaskFailurePolicy: taskFailurePolicyHalt,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, edgeSvc, engine)

	// The job should complete without error (all_failed task is simply skipped)
	err := New(&models.Job{ID: jobID}, opts...).Run(context.Background())
	require.NoError(t, err)

	snapshot := latestRunSnapshot(t, store, jobID)
	status := taskStatusByID(snapshot)
	require.Equal(t, run.TaskStatusSucceeded, status[taskUpstream])
	require.Equal(t, run.TaskStatusSkipped, status[taskOnFailure], "all_failed task should be skipped when upstream succeeded")
}

// TestOneSuccessTaskRunsWhenAtLeastOneSucceeds verifies one_success behaviour.
func TestOneSuccessTaskRunsWhenAtLeastOneSucceeds(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskA := uuid.New() // will succeed
	taskB := uuid.New() // will fail
	taskJoin := uuid.New()

	taskSvc := &fakeTaskService{tasks: models.Tasks{
		{ID: taskA, JobID: jobID, AtomID: uuid.New(), TriggerRule: jobdefschema.TriggerRuleAllSuccess},
		{ID: taskB, JobID: jobID, AtomID: uuid.New(), TriggerRule: jobdefschema.TriggerRuleAllSuccess},
		{ID: taskJoin, JobID: jobID, AtomID: uuid.New(), TriggerRule: jobdefschema.TriggerRuleOneSuccess},
	}}
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{
		taskSvc.tasks[0].AtomID: fakeModelAtom(taskSvc.tasks[0].AtomID),
		taskSvc.tasks[1].AtomID: fakeModelAtom(taskSvc.tasks[1].AtomID),
		taskSvc.tasks[2].AtomID: fakeModelAtom(taskSvc.tasks[2].AtomID),
	}}
	edgeSvc := &fakeTaskEdgeService{edges: models.TaskEdges{
		{ID: uuid.New(), JobID: jobID, FromTaskID: taskA, ToTaskID: taskJoin},
		{ID: uuid.New(), JobID: jobID, FromTaskID: taskB, ToTaskID: taskJoin},
	}}
	persistGraph(t, db, taskSvc.tasks, edgeSvc.edges)

	// taskA succeeds, taskB fails
	engine.runDurationByName[taskA.String()] = 10 * time.Millisecond
	engine.createErrByName[taskB.String()] = errors.New("b failed")

	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  2,
		TaskFailurePolicy: taskFailurePolicyContinue,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, edgeSvc, engine)

	err := New(&models.Job{ID: jobID}, opts...).Run(context.Background())
	require.Error(t, err) // b failed → runErr is set

	snapshot := latestRunSnapshot(t, store, jobID)
	status := taskStatusByID(snapshot)
	require.Equal(t, run.TaskStatusSucceeded, status[taskA])
	require.Equal(t, run.TaskStatusFailed, status[taskB])
	require.Equal(t, run.TaskStatusSucceeded, status[taskJoin], "one_success join should run because taskA succeeded")
}

// TestOneSuccessSkippedWhenAllFail verifies one_success is skipped when no
// predecessor succeeded.
func TestOneSuccessSkippedWhenAllFail(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskA := uuid.New()
	taskB := uuid.New()
	taskJoin := uuid.New()

	taskSvc := &fakeTaskService{tasks: models.Tasks{
		{ID: taskA, JobID: jobID, AtomID: uuid.New(), TriggerRule: jobdefschema.TriggerRuleAllSuccess},
		{ID: taskB, JobID: jobID, AtomID: uuid.New(), TriggerRule: jobdefschema.TriggerRuleAllSuccess},
		{ID: taskJoin, JobID: jobID, AtomID: uuid.New(), TriggerRule: jobdefschema.TriggerRuleOneSuccess},
	}}
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{
		taskSvc.tasks[0].AtomID: fakeModelAtom(taskSvc.tasks[0].AtomID),
		taskSvc.tasks[1].AtomID: fakeModelAtom(taskSvc.tasks[1].AtomID),
		taskSvc.tasks[2].AtomID: fakeModelAtom(taskSvc.tasks[2].AtomID),
	}}
	edgeSvc := &fakeTaskEdgeService{edges: models.TaskEdges{
		{ID: uuid.New(), JobID: jobID, FromTaskID: taskA, ToTaskID: taskJoin},
		{ID: uuid.New(), JobID: jobID, FromTaskID: taskB, ToTaskID: taskJoin},
	}}
	persistGraph(t, db, taskSvc.tasks, edgeSvc.edges)

	engine.createErrByName[taskA.String()] = errors.New("a failed")
	engine.createErrByName[taskB.String()] = errors.New("b failed")

	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  2,
		TaskFailurePolicy: taskFailurePolicyContinue,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, edgeSvc, engine)

	err := New(&models.Job{ID: jobID}, opts...).Run(context.Background())
	require.Error(t, err)

	snapshot := latestRunSnapshot(t, store, jobID)
	status := taskStatusByID(snapshot)
	require.Equal(t, run.TaskStatusFailed, status[taskA])
	require.Equal(t, run.TaskStatusFailed, status[taskB])
	require.Equal(t, run.TaskStatusSkipped, status[taskJoin], "one_success join should be skipped when all predecessors failed")
}

// TestAlwaysTaskRunsRegardlessOfUpstreamStatus is an alias-rule integration
// test verifying that "always" behaves like "all_done".
func TestAlwaysTaskRunsRegardlessOfUpstreamStatus(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskUpstream := uuid.New()
	taskNotify := uuid.New() // always — should run no matter what

	taskSvc := &fakeTaskService{tasks: models.Tasks{
		{ID: taskUpstream, JobID: jobID, AtomID: uuid.New(), TriggerRule: jobdefschema.TriggerRuleAllSuccess},
		{ID: taskNotify, JobID: jobID, AtomID: uuid.New(), TriggerRule: jobdefschema.TriggerRuleAlways},
	}}
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{
		taskSvc.tasks[0].AtomID: fakeModelAtom(taskSvc.tasks[0].AtomID),
		taskSvc.tasks[1].AtomID: fakeModelAtom(taskSvc.tasks[1].AtomID),
	}}
	edgeSvc := &fakeTaskEdgeService{edges: models.TaskEdges{
		{ID: uuid.New(), JobID: jobID, FromTaskID: taskUpstream, ToTaskID: taskNotify},
	}}
	persistGraph(t, db, taskSvc.tasks, edgeSvc.edges)

	engine.createErrByName[taskUpstream.String()] = errors.New("upstream exploded")

	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  1,
		TaskFailurePolicy: taskFailurePolicyContinue,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, edgeSvc, engine)

	err := New(&models.Job{ID: jobID}, opts...).Run(context.Background())
	require.Error(t, err)

	snapshot := latestRunSnapshot(t, store, jobID)
	status := taskStatusByID(snapshot)
	require.Equal(t, run.TaskStatusFailed, status[taskUpstream])
	require.Equal(t, run.TaskStatusSucceeded, status[taskNotify], "always task must run regardless of upstream failure")
}

// TestMixedRuleDAG exercises a realistic cleanup-on-failure DAG:
//
//	process → notify_always
//	process → cleanup_on_fail (all_failed)
//
// Happy path: process succeeds → notify runs, cleanup is skipped.
// Failure path: process fails → notify runs, cleanup runs.
func TestMixedRuleDAGHappyPath(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskProcess := uuid.New()
	taskNotify := uuid.New()
	taskCleanup := uuid.New()

	taskSvc := &fakeTaskService{tasks: models.Tasks{
		{ID: taskProcess, JobID: jobID, AtomID: uuid.New(), TriggerRule: jobdefschema.TriggerRuleAllSuccess},
		{ID: taskNotify, JobID: jobID, AtomID: uuid.New(), TriggerRule: jobdefschema.TriggerRuleAlways},
		{ID: taskCleanup, JobID: jobID, AtomID: uuid.New(), TriggerRule: jobdefschema.TriggerRuleAllFailed},
	}}
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{
		taskSvc.tasks[0].AtomID: fakeModelAtom(taskSvc.tasks[0].AtomID),
		taskSvc.tasks[1].AtomID: fakeModelAtom(taskSvc.tasks[1].AtomID),
		taskSvc.tasks[2].AtomID: fakeModelAtom(taskSvc.tasks[2].AtomID),
	}}
	edgeSvc := &fakeTaskEdgeService{edges: models.TaskEdges{
		{ID: uuid.New(), JobID: jobID, FromTaskID: taskProcess, ToTaskID: taskNotify},
		{ID: uuid.New(), JobID: jobID, FromTaskID: taskProcess, ToTaskID: taskCleanup},
	}}
	persistGraph(t, db, taskSvc.tasks, edgeSvc.edges)

	// process succeeds
	engine.runDurationByName[taskProcess.String()] = 10 * time.Millisecond

	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  1,
		TaskFailurePolicy: taskFailurePolicyContinue,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, edgeSvc, engine)

	err := New(&models.Job{ID: jobID}, opts...).Run(context.Background())
	require.NoError(t, err)

	snapshot := latestRunSnapshot(t, store, jobID)
	status := taskStatusByID(snapshot)
	require.Equal(t, run.TaskStatusSucceeded, status[taskProcess], "process should succeed")
	require.Equal(t, run.TaskStatusSucceeded, status[taskNotify], "always notify should run on success")
	require.Equal(t, run.TaskStatusSkipped, status[taskCleanup], "all_failed cleanup should be skipped on success")
}

func TestMixedRuleDAGFailurePath(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskProcess := uuid.New()
	taskNotify := uuid.New()
	taskCleanup := uuid.New()

	taskSvc := &fakeTaskService{tasks: models.Tasks{
		{ID: taskProcess, JobID: jobID, AtomID: uuid.New(), TriggerRule: jobdefschema.TriggerRuleAllSuccess},
		{ID: taskNotify, JobID: jobID, AtomID: uuid.New(), TriggerRule: jobdefschema.TriggerRuleAlways},
		{ID: taskCleanup, JobID: jobID, AtomID: uuid.New(), TriggerRule: jobdefschema.TriggerRuleAllFailed},
	}}
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{
		taskSvc.tasks[0].AtomID: fakeModelAtom(taskSvc.tasks[0].AtomID),
		taskSvc.tasks[1].AtomID: fakeModelAtom(taskSvc.tasks[1].AtomID),
		taskSvc.tasks[2].AtomID: fakeModelAtom(taskSvc.tasks[2].AtomID),
	}}
	edgeSvc := &fakeTaskEdgeService{edges: models.TaskEdges{
		{ID: uuid.New(), JobID: jobID, FromTaskID: taskProcess, ToTaskID: taskNotify},
		{ID: uuid.New(), JobID: jobID, FromTaskID: taskProcess, ToTaskID: taskCleanup},
	}}
	persistGraph(t, db, taskSvc.tasks, edgeSvc.edges)

	// process fails
	engine.createErrByName[taskProcess.String()] = errors.New("process exploded")

	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  1,
		TaskFailurePolicy: taskFailurePolicyContinue,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, edgeSvc, engine)

	err := New(&models.Job{ID: jobID}, opts...).Run(context.Background())
	require.Error(t, err)

	snapshot := latestRunSnapshot(t, store, jobID)
	status := taskStatusByID(snapshot)
	require.Equal(t, run.TaskStatusFailed, status[taskProcess], "process should fail")
	require.Equal(t, run.TaskStatusSucceeded, status[taskNotify], "always notify must still run on failure")
	require.Equal(t, run.TaskStatusSucceeded, status[taskCleanup], "all_failed cleanup must run when process failed")
}

func TestSkippedTaskPropagatesToDescendants(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskProcess := uuid.New()
	taskCleanup := uuid.New()
	taskNotify := uuid.New()

	taskSvc := &fakeTaskService{tasks: models.Tasks{
		{ID: taskProcess, JobID: jobID, AtomID: uuid.New(), TriggerRule: jobdefschema.TriggerRuleAllSuccess},
		{ID: taskCleanup, JobID: jobID, AtomID: uuid.New(), TriggerRule: jobdefschema.TriggerRuleAllFailed},
		{ID: taskNotify, JobID: jobID, AtomID: uuid.New(), TriggerRule: jobdefschema.TriggerRuleAlways},
	}}
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{
		taskSvc.tasks[0].AtomID: fakeModelAtom(taskSvc.tasks[0].AtomID),
		taskSvc.tasks[1].AtomID: fakeModelAtom(taskSvc.tasks[1].AtomID),
		taskSvc.tasks[2].AtomID: fakeModelAtom(taskSvc.tasks[2].AtomID),
	}}
	edgeSvc := &fakeTaskEdgeService{edges: models.TaskEdges{
		{ID: uuid.New(), JobID: jobID, FromTaskID: taskProcess, ToTaskID: taskCleanup},
		{ID: uuid.New(), JobID: jobID, FromTaskID: taskCleanup, ToTaskID: taskNotify},
	}}
	persistGraph(t, db, taskSvc.tasks, edgeSvc.edges)

	engine.runDurationByName[taskProcess.String()] = 10 * time.Millisecond

	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  1,
		TaskFailurePolicy: taskFailurePolicyContinue,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, edgeSvc, engine)

	err := New(&models.Job{ID: jobID}, opts...).Run(context.Background())
	require.NoError(t, err)

	snapshot := latestRunSnapshot(t, store, jobID)
	status := taskStatusByID(snapshot)
	require.Equal(t, run.TaskStatusSucceeded, status[taskProcess], "process should succeed")
	require.Equal(t, run.TaskStatusSkipped, status[taskCleanup], "all_failed cleanup should be skipped on success")
	require.Equal(t, run.TaskStatusSucceeded, status[taskNotify], "always notify should run after skipped cleanup")
}
