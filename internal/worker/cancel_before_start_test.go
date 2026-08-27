package worker

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// seedClaimedTaskRun builds a run with one claimed, unstarted task and returns
// the row exactly as a worker pool holds it: claimed and running, from before
// anything else touched it.
func seedClaimedTaskRun(t *testing.T) (*run.Store, *gorm.DB, *models.TaskRun) {
	t.Helper()
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	now := time.Now().UTC()

	trigger := &models.Trigger{ID: uuid.New(), Alias: "cbs-trig-" + uuid.NewString()[:8], Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(trigger).Error)
	job := &models.Job{ID: uuid.New(), Alias: "cbs-job-" + uuid.NewString()[:8], TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(job).Error)
	atomModel := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(atomModel).Error)
	task := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atomModel.ID, Name: "process", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(task).Error)

	jobRun := &models.JobRun{
		ID: uuid.New(), JobID: job.ID, TriggerID: trigger.ID, TriggerType: string(trigger.Type),
		Status: string(run.StatusRunning), StartedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, db.Create(jobRun).Error)

	taskRun := &models.TaskRun{
		ID: uuid.New(), JobRunID: jobRun.ID, TaskID: task.ID, AtomID: atomModel.ID,
		Engine: atomModel.Engine, Image: atomModel.Image, Command: atomModel.Command,
		Status: string(run.TaskStatusRunning), ClaimedBy: "worker-1", Attempt: 1, MaxAttempts: 1,
		PartitionValue: "gate", PartitionIndex: 1, PartitionCount: 3,
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, db.Create(taskRun).Error)
	return store, db, taskRun
}

// TestRuntimeExecutorDoesNotStartACancelledInstance is the worker half of
// cancel-before-start: a claimed instance that fail_fast resolved while it
// waited for a pool slot must never reach the runtime, and must post nothing.
//
// The executor is handed the row as the pool captured it — still running, still
// claimed — because that is what actually happens: the cancel lands in the
// database, not in the in-flight struct.
func TestRuntimeExecutorDoesNotStartACancelledInstance(t *testing.T) {
	store, db, taskRun := seedClaimedTaskRun(t)

	// fail_fast cancelled it while it sat in the pool: terminal row, claim
	// revoked (internal/run: markInstanceCancelledBeforeStartTx).
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", taskRun.ID).Updates(map[string]any{
		"status":            string(run.TaskStatusSkipped),
		"error":             "fan-out group failed fast",
		"claimed_by":        "",
		"claim_expires_at":  nil,
		"started_at":        nil,
		"terminal_sequence": 7,
	}).Error)

	engine := &captureCreateEngine{}
	executor := &runtimeExecutor{
		store:     store,
		localSink: NewLocalSink(store),
		engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) {
			return engine, nil
		},
	}
	// The stale copy the pool holds, claim and all.
	executor.Execute(context.Background(), taskRun)

	require.Nil(t, engine.createReq,
		"a cancelled instance must not reach the runtime: engine.Create both creates AND starts the container")

	var after models.TaskRun
	require.NoError(t, db.First(&after, "id = ?", taskRun.ID).Error)
	require.Equal(t, string(run.TaskStatusSkipped), after.Status,
		"the worker must post nothing for a task that was resolved out from under it")
	require.Equal(t, "fan-out group failed fast", after.Error, "and must not overwrite the cancel reason")
	require.Equal(t, int64(7), after.TerminalSequence)
	require.Empty(t, after.RuntimeID)
}

// TestRuntimeExecutorStopsAContainerCancelledDuringCreate covers the narrower
// race the pre-flight check cannot close: the cancel commits while Create is in
// flight. StartTaskClaimed then refuses, and the container that Create started
// must be torn down rather than left running against a terminal row.
func TestRuntimeExecutorStopsAContainerCancelledDuringCreate(t *testing.T) {
	store, db, taskRun := seedClaimedTaskRun(t)

	engine := &cancelOnCreateEngine{
		onCreate: func() {
			require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", taskRun.ID).Updates(map[string]any{
				"status":            string(run.TaskStatusSkipped),
				"error":             "fan-out group failed fast",
				"claimed_by":        "",
				"terminal_sequence": 9,
			}).Error)
		},
	}
	executor := &runtimeExecutor{
		store:     store,
		localSink: NewLocalSink(store),
		engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) {
			return engine, nil
		},
	}
	executor.Execute(context.Background(), taskRun)

	require.True(t, engine.stopped, "a container started for a task cancelled mid-create must be stopped, not orphaned")
	require.False(t, engine.waited, "and must not be waited on as if it were this task's real execution")

	var after models.TaskRun
	require.NoError(t, db.First(&after, "id = ?", taskRun.ID).Error)
	require.Equal(t, string(run.TaskStatusSkipped), after.Status)
	require.Equal(t, int64(9), after.TerminalSequence)
}

// cancelOnCreateEngine runs onCreate between the pre-flight check and the
// StartTaskClaimed fence, reproducing a cancel that commits while the container
// is being created.
type cancelOnCreateEngine struct {
	onCreate func()
	stopped  bool
	waited   bool
}

func (e *cancelOnCreateEngine) Get(*atom.EngineGetRequest) (atom.Atom, error) {
	return &fakeMonitorAtom{id: "runtime", result: atom.Success}, nil
}

func (e *cancelOnCreateEngine) List(*atom.EngineListRequest) ([]atom.Atom, error) { return nil, nil }

func (e *cancelOnCreateEngine) Create(*atom.EngineCreateRequest) (atom.Atom, error) {
	if e.onCreate != nil {
		e.onCreate()
	}
	return &fakeMonitorAtom{id: "runtime", result: atom.Unknown}, nil
}

func (e *cancelOnCreateEngine) Wait(*atom.EngineWaitRequest) (atom.Atom, error) {
	e.waited = true
	return &fakeMonitorAtom{id: "runtime", result: atom.Success}, nil
}

func (e *cancelOnCreateEngine) Stop(*atom.EngineStopRequest) error {
	e.stopped = true
	return nil
}

func (e *cancelOnCreateEngine) Logs(*atom.EngineLogsRequest) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
