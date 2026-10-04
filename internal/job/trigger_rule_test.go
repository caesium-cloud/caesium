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

type triggerRuleRunScenario struct {
	name              string
	taskNames         []string
	triggerRules      map[string]string
	edges             [][2]string
	failurePolicy     string
	maxParallel       int
	createErrByName   map[string]string
	runDurationByName map[string]time.Duration
	wantRunErr        bool
	wantRunError      string
	wantStatuses      map[string]run.TaskStatus
}

func runTriggerRuleScenario(t *testing.T, tc triggerRuleRunScenario) {
	t.Helper()
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
	store := run.NewStore(db)
	engine := newFakeEngine()
	jobID := uuid.New()
	taskIDs := make(map[string]uuid.UUID, len(tc.taskNames))
	atomIDs := make(map[string]uuid.UUID, len(tc.taskNames))
	tasks := make(models.Tasks, 0, len(tc.taskNames))
	atoms := make(map[uuid.UUID]*models.Atom, len(tc.taskNames))
	for _, name := range tc.taskNames {
		taskIDs[name] = uuid.New()
		atomIDs[name] = uuid.New()
		tasks = append(tasks, models.Task{
			ID: taskIDs[name], JobID: jobID, AtomID: atomIDs[name], TriggerRule: tc.triggerRules[name],
		})
		atoms[atomIDs[name]] = fakeModelAtom(atomIDs[name])
	}
	edges := make(models.TaskEdges, 0, len(tc.edges))
	for _, edge := range tc.edges {
		edges = append(edges, models.TaskEdge{
			ID: uuid.New(), JobID: jobID, FromTaskID: taskIDs[edge[0]], ToTaskID: taskIDs[edge[1]],
		})
	}
	persistGraph(t, db, tasks, edges)
	for name, message := range tc.createErrByName {
		engine.createErrByName[taskIDs[name].String()] = errors.New(message)
	}
	for name, duration := range tc.runDurationByName {
		engine.runDurationByName[taskIDs[name].String()] = duration
	}
	taskSvc := &fakeTaskService{tasks: tasks}
	atomSvc := &fakeAtomService{atoms: atoms}
	edgeSvc := &fakeTaskEdgeService{edges: edges}
	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  tc.maxParallel,
		TaskFailurePolicy: tc.failurePolicy,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, edgeSvc, engine)

	err := New(&models.Job{ID: jobID}, opts...).Run(context.Background())
	if tc.wantRunErr {
		require.Error(t, err)
		if tc.wantRunError != "" {
			require.Contains(t, err.Error(), tc.wantRunError)
		}
	} else {
		require.NoError(t, err)
	}
	status := taskStatusByID(latestRunSnapshot(t, store, jobID))
	for name, want := range tc.wantStatuses {
		require.Equal(t, want, status[taskIDs[name]], "%s task status", name)
	}
}

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

func TestAllFailedTriggerRule(t *testing.T) {
	for _, tc := range []triggerRuleRunScenario{
		{
			name: "runs when upstream fails", taskNames: []string{"upstream", "on_failure"},
			triggerRules: map[string]string{"upstream": jobdefschema.TriggerRuleAllSuccess, "on_failure": jobdefschema.TriggerRuleAllFailed},
			edges:        [][2]string{{"upstream", "on_failure"}}, failurePolicy: taskFailurePolicyContinue, maxParallel: 1,
			createErrByName: map[string]string{"upstream": "upstream failed"}, wantRunErr: true, wantRunError: "upstream failed",
			wantStatuses: map[string]run.TaskStatus{"upstream": run.TaskStatusFailed, "on_failure": run.TaskStatusSucceeded},
		},
		{
			name: "skips when upstream succeeds", taskNames: []string{"upstream", "on_failure"},
			triggerRules: map[string]string{"upstream": jobdefschema.TriggerRuleAllSuccess, "on_failure": jobdefschema.TriggerRuleAllFailed},
			edges:        [][2]string{{"upstream", "on_failure"}}, failurePolicy: taskFailurePolicyHalt, maxParallel: 1,
			runDurationByName: map[string]time.Duration{"upstream": 10 * time.Millisecond},
			wantStatuses:      map[string]run.TaskStatus{"upstream": run.TaskStatusSucceeded, "on_failure": run.TaskStatusSkipped},
		},
	} {
		t.Run(tc.name, func(t *testing.T) { runTriggerRuleScenario(t, tc) })
	}
}

func TestOneSuccessTriggerRule(t *testing.T) {
	for _, tc := range []triggerRuleRunScenario{
		{
			name:      "runs when at least one predecessor succeeds",
			taskNames: []string{"task_a", "task_b", "join"},
			triggerRules: map[string]string{
				"task_a": jobdefschema.TriggerRuleAllSuccess,
				"task_b": jobdefschema.TriggerRuleAllSuccess,
				"join":   jobdefschema.TriggerRuleOneSuccess,
			},
			edges:         [][2]string{{"task_a", "join"}, {"task_b", "join"}},
			failurePolicy: taskFailurePolicyContinue, maxParallel: 2,
			createErrByName:   map[string]string{"task_b": "b failed"},
			runDurationByName: map[string]time.Duration{"task_a": 10 * time.Millisecond},
			wantRunErr:        true, wantRunError: "b failed",
			wantStatuses: map[string]run.TaskStatus{
				"task_a": run.TaskStatusSucceeded, "task_b": run.TaskStatusFailed, "join": run.TaskStatusSucceeded,
			},
		},
		{
			name:      "skips when all predecessors fail",
			taskNames: []string{"task_a", "task_b", "join"},
			triggerRules: map[string]string{
				"task_a": jobdefschema.TriggerRuleAllSuccess,
				"task_b": jobdefschema.TriggerRuleAllSuccess,
				"join":   jobdefschema.TriggerRuleOneSuccess,
			},
			edges:         [][2]string{{"task_a", "join"}, {"task_b", "join"}},
			failurePolicy: taskFailurePolicyContinue, maxParallel: 2,
			createErrByName: map[string]string{"task_a": "a failed", "task_b": "b failed"},
			wantRunErr:      true,
			wantStatuses: map[string]run.TaskStatus{
				"task_a": run.TaskStatusFailed, "task_b": run.TaskStatusFailed, "join": run.TaskStatusSkipped,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) { runTriggerRuleScenario(t, tc) })
	}
}

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

// TestMixedRuleDAG exercises cleanup-on-failure propagation on both the happy
// and failure paths of the same process → notify/cleanup graph.
func TestMixedRuleDAG(t *testing.T) {
	for _, tc := range []triggerRuleRunScenario{
		{
			name: "happy path", taskNames: []string{"process", "notify", "cleanup"},
			triggerRules: map[string]string{
				"process": jobdefschema.TriggerRuleAllSuccess,
				"notify":  jobdefschema.TriggerRuleAlways,
				"cleanup": jobdefschema.TriggerRuleAllFailed,
			},
			edges:         [][2]string{{"process", "notify"}, {"process", "cleanup"}},
			failurePolicy: taskFailurePolicyContinue, maxParallel: 1,
			runDurationByName: map[string]time.Duration{"process": 10 * time.Millisecond},
			wantStatuses: map[string]run.TaskStatus{
				"process": run.TaskStatusSucceeded, "notify": run.TaskStatusSucceeded, "cleanup": run.TaskStatusSkipped,
			},
		},
		{
			name: "failure path", taskNames: []string{"process", "notify", "cleanup"},
			triggerRules: map[string]string{
				"process": jobdefschema.TriggerRuleAllSuccess,
				"notify":  jobdefschema.TriggerRuleAlways,
				"cleanup": jobdefschema.TriggerRuleAllFailed,
			},
			edges:         [][2]string{{"process", "notify"}, {"process", "cleanup"}},
			failurePolicy: taskFailurePolicyContinue, maxParallel: 1,
			createErrByName: map[string]string{"process": "process exploded"}, wantRunErr: true, wantRunError: "process exploded",
			wantStatuses: map[string]run.TaskStatus{
				"process": run.TaskStatusFailed, "notify": run.TaskStatusSucceeded, "cleanup": run.TaskStatusSucceeded,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) { runTriggerRuleScenario(t, tc) })
	}
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
