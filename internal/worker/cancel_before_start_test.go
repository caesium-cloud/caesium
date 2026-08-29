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
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
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
		Status: string(run.TaskStatusRunning), ClaimedBy: "worker-1", ClaimAttempt: 1, Attempt: 1, MaxAttempts: 1,
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

// TestRuntimeExecutorRefusesStaleAttemptBeforeCreate reproduces a claim that
// expires while the old goroutine is still preparing its runtime. The same
// worker receives attempt 2 for the same row. Worker identity alone would let
// attempt 1 create a duplicate container and overwrite attempt 2's runtime id;
// the claim-attempt fence must stop it before engine.Create.
func TestRuntimeExecutorRefusesStaleAttemptBeforeCreate(t *testing.T) {
	store, db, stale := seedClaimedTaskRun(t)
	enteredFactory := make(chan struct{})
	resumeFactory := make(chan struct{})
	engine := &captureCreateEngine{}
	executor := &runtimeExecutor{
		store:     store,
		localSink: NewLocalSink(store),
		engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) {
			close(enteredFactory)
			<-resumeFactory
			return engine, nil
		},
	}

	done := make(chan struct{})
	go func() {
		executor.Execute(context.Background(), stale)
		close(done)
	}()
	select {
	case <-enteredFactory:
	case <-time.After(2 * time.Second):
		t.Fatal("stale executor did not reach the pre-create gate")
	}

	// The lease expired and the dispatcher assigned the row back to this same
	// node. ClaimAttempt is the only identity component that changed.
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", stale.ID).Updates(map[string]any{
		"status":           string(run.TaskStatusRunning),
		"claimed_by":       stale.ClaimedBy,
		"claim_attempt":    2,
		"runtime_id":       "attempt-2-runtime",
		"claim_expires_at": time.Now().UTC().Add(time.Minute),
	}).Error)
	close(resumeFactory)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stale executor did not return")
	}

	require.Nil(t, engine.createReq, "attempt 1 must not create a container for attempt 2's claim")
	var current models.TaskRun
	require.NoError(t, db.First(&current, "id = ?", stale.ID).Error)
	require.Equal(t, 2, current.ClaimAttempt)
	require.Equal(t, "attempt-2-runtime", current.RuntimeID,
		"stale pre-start work must not overwrite the current attempt's runtime")
}

func TestClaimOwnedMutationsRejectStaleSameNodeAttempt(t *testing.T) {
	exitCode := 42
	mutations := map[string]func(*run.Store, *models.TaskRun) error{
		"ensure startable": func(s *run.Store, tr *models.TaskRun) error {
			return s.EnsureTaskRunStartableAttempt(tr.JobRunID, tr.ID, tr.ClaimedBy, tr.ClaimAttempt)
		},
		"start": func(s *run.Store, tr *models.TaskRun) error {
			return s.StartTaskClaimedAttempt(tr.JobRunID, tr.ID, "stale-runtime", tr.ClaimedBy, tr.ClaimAttempt)
		},
		"retry": func(s *run.Store, tr *models.TaskRun) error {
			return s.RetryTaskClaimedInstanceAttempt(tr.JobRunID, tr.ID, 2, tr.ClaimedBy, tr.ClaimAttempt)
		},
		"hash": func(s *run.Store, tr *models.TaskRun) error {
			return s.SetTaskHashWithBlobClaimedAttempt(tr.JobRunID, tr.ID, "stale-hash", "sha256:stale", []byte(`{"stale":true}`), tr.ClaimedBy, tr.ClaimAttempt)
		},
		"descriptor inputs": func(s *run.Store, tr *models.TaskRun) error {
			return s.UpdateTaskExecutionDescriptorInputsClaimedAttempt(tr.JobRunID, tr.ID, nil, nil, "stale-hash", "", nil, tr.ClaimedBy, tr.ClaimAttempt)
		},
		"descriptor secrets": func(s *run.Store, tr *models.TaskRun) error {
			return s.UpdateTaskExecutionDescriptorSecretRefsClaimedAttempt(tr.JobRunID, tr.ID,
				[]models.TaskExecutionSecretRef{{EnvKey: "TOKEN", Ref: "secret://env/TOKEN"}}, tr.ClaimedBy, tr.ClaimAttempt)
		},
		"exit code": func(s *run.Store, tr *models.TaskRun) error {
			return s.SetTaskExitCodeClaimedAttempt(tr.JobRunID, tr.ID, &exitCode, tr.ClaimedBy, tr.ClaimAttempt)
		},
		"log": func(s *run.Store, tr *models.TaskRun) error {
			return s.SaveTaskLogSnapshotClaimedAttempt(tr.JobRunID, tr.ID,
				&run.TaskLogSnapshot{Text: "stale log", Truncated: true}, tr.ClaimedBy, tr.ClaimAttempt)
		},
		"schema violations": func(s *run.Store, tr *models.TaskRun) error {
			return s.SaveSchemaViolationsClaimedAttempt(tr.JobRunID, tr.ID,
				[]pkgtask.SchemaViolation{{Key: "x", Message: "stale"}}, tr.ClaimedBy, tr.ClaimAttempt)
		},
		"effective hash": func(s *run.Store, tr *models.TaskRun) error {
			return s.SetTaskEffectiveHashClaimedAttempt(tr.JobRunID, tr.ID, "stale-effective", tr.ClaimedBy, tr.ClaimAttempt)
		},
	}

	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			store, db, stale := seedClaimedTaskRun(t)
			require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", stale.ID).Updates(map[string]any{
				"claim_attempt": 2,
				"runtime_id":    "attempt-2-runtime",
			}).Error)
			require.ErrorIs(t, mutate(store, stale), run.ErrTaskClaimMismatch)

			var current models.TaskRun
			require.NoError(t, db.First(&current, "id = ?", stale.ID).Error)
			require.Equal(t, 2, current.ClaimAttempt)
			require.Equal(t, "attempt-2-runtime", current.RuntimeID)
			require.Equal(t, 1, current.Attempt)
			require.Empty(t, current.Hash)
			require.Empty(t, current.EffectiveHash)
			require.Nil(t, current.ExitCode)
			require.Empty(t, current.LogText)
			require.Empty(t, current.SchemaViolations)
			require.Empty(t, current.ExecutionDescriptor)
		})
	}
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
