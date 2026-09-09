package worker

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// haltWorkerFixture seeds the smallest DAG that separates the three things a
// halt has to tell apart, with `fail` claimed by this worker exactly as the
// pool holds it:
//
//	fail (claimed, running) ──▶ cleanup     (all_done)
//	independent               (root, all_success, pending)
type haltWorkerFixture struct {
	store       *run.Store
	db          *gorm.DB
	runID       uuid.UUID
	fail        *models.TaskRun
	cleanup     uuid.UUID
	independent uuid.UUID
}

func newHaltWorkerFixture(t *testing.T) *haltWorkerFixture {
	t.Helper()
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
	store := run.NewStore(db)
	now := time.Now().UTC()

	trigger := &models.Trigger{ID: uuid.New(), Alias: "halt-trig-" + uuid.NewString()[:8], Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(trigger).Error)
	job := &models.Job{ID: uuid.New(), Alias: "halt-job-" + uuid.NewString()[:8], TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(job).Error)
	atomModel := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(atomModel).Error)

	mk := func(name, rule string, position int) *models.Task {
		task := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atomModel.ID, Name: name, Position: position, TriggerRule: rule, CreatedAt: now, UpdatedAt: now}
		require.NoError(t, db.Create(task).Error)
		return task
	}
	fail := mk("fail", jobdefschema.TriggerRuleAllSuccess, 0)
	cleanup := mk("cleanup", jobdefschema.TriggerRuleAllDone, 1)
	independent := mk("independent", jobdefschema.TriggerRuleAllSuccess, 2)
	require.NoError(t, db.Create(&models.TaskEdge{ID: uuid.New(), JobID: job.ID, FromTaskID: fail.ID, ToTaskID: cleanup.ID, CreatedAt: now, UpdatedAt: now}).Error)

	jobRun := &models.JobRun{
		ID: uuid.New(), JobID: job.ID, TriggerID: trigger.ID, TriggerType: string(trigger.Type),
		Status: string(run.StatusRunning), StartedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, db.Create(jobRun).Error)

	row := func(task *models.Task, status string, outstanding int, claimedBy string) *models.TaskRun {
		tr := &models.TaskRun{
			ID: uuid.New(), JobRunID: jobRun.ID, TaskID: task.ID, AtomID: atomModel.ID,
			Engine: atomModel.Engine, Image: atomModel.Image, Command: atomModel.Command,
			Status: status, ClaimedBy: claimedBy, Attempt: 1, MaxAttempts: 1,
			OutstandingPredecessors: outstanding, CreatedAt: now, UpdatedAt: now,
		}
		require.NoError(t, db.Create(tr).Error)
		return tr
	}
	failRow := row(fail, string(run.TaskStatusRunning), 0, "worker-1")
	row(cleanup, string(run.TaskStatusPending), 1, "")
	row(independent, string(run.TaskStatusPending), 0, "")

	return &haltWorkerFixture{store: store, db: db, runID: jobRun.ID, fail: failRow, cleanup: cleanup.ID, independent: independent.ID}
}

func (f *haltWorkerFixture) row(t *testing.T, taskID uuid.UUID) models.TaskRun {
	t.Helper()
	var tr models.TaskRun
	require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ?", f.runID, taskID).First(&tr).Error)
	return tr
}

func (f *haltWorkerFixture) execute(t *testing.T, continueOnFailure bool) {
	t.Helper()
	// The container runs and exits non-zero: the final attempt reports its
	// failure RESULT through the completion route, which is what actually
	// terminalizes the row — the later sink.Failed delivery finds it terminal.
	engine := &attemptResultEngine{results: []atom.Result{atom.Failure}}
	executor := &runtimeExecutor{
		store:             f.store,
		localSink:         NewLocalSink(f.store),
		continueOnFailure: continueOnFailure,
		engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) {
			return engine, nil
		},
	}
	executor.Execute(context.Background(), f.fail)
	require.Equal(t, 1, engine.createCount())
	require.Equal(t, string(run.TaskStatusFailed), f.row(t, f.fail.TaskID).Status, "precondition: the task must have failed")
}

// Under `halt` the worker that records a failure also halts the run: every
// step that has not started and is not failure-tolerant is skipped before the
// owner's next dispatch tick can claim it. The failed task's own `all_done`
// successor is NOT halted — the failure transaction released it, and the
// halt leaves it dispatchable.
func TestRuntimeExecutorHaltSkipsUnstartedIntolerantSteps(t *testing.T) {
	f := newHaltWorkerFixture(t)
	f.execute(t, false)

	independent := f.row(t, f.independent)
	require.Equal(t, string(run.TaskStatusSkipped), independent.Status)
	require.Equal(t, run.HaltReason("fail"), independent.Error)

	cleanup := f.row(t, f.cleanup)
	require.Equal(t, string(run.TaskStatusPending), cleanup.Status)
	require.Zero(t, cleanup.OutstandingPredecessors, "the released all_done successor stays dispatchable")
}

// Under `continue` the worker sweeps only the failed task's intolerant
// descendants; independent work is left to run.
func TestRuntimeExecutorContinueLeavesIndependentWorkAlone(t *testing.T) {
	f := newHaltWorkerFixture(t)
	f.execute(t, true)

	require.Equal(t, string(run.TaskStatusPending), f.row(t, f.independent).Status)
	cleanup := f.row(t, f.cleanup)
	require.Equal(t, string(run.TaskStatusPending), cleanup.Status)
	require.Zero(t, cleanup.OutstandingPredecessors)
}
