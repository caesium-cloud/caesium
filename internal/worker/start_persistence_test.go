package worker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Exercise Execute through the real start transaction: the rolling-upgrade
// failure happened after Create, when migrations held the write lock longer
// than the store's short contention budget. Retrying the atom or reporting the
// database error as a task result loses already-started work.
func TestRuntimeExecutorStartPersistenceOutlastsContentionBudget(t *testing.T) {
	store, db, taskRun := seedClaimedTaskRun(t)
	started := time.Time{}
	creates, writes := 0, 0
	engine := &startPersistenceEngine{cancelOnCreateEngine: cancelOnCreateEngine{onCreate: func() {
		creates++
		started = time.Now()
	}}}
	injectStartWriteError(t, db, func() error {
		writes++
		if time.Since(started) < 3*time.Second {
			return errors.New("database is locked")
		}
		return nil
	})
	sink := &fakeSink{}
	executor := &runtimeExecutor{
		store: store, localSink: sink, taskTimeout: 10 * time.Second,
		engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) { return engine, nil },
	}
	executor.Execute(t.Context(), taskRun)

	require.Greater(t, writes, 9, "must exhaust the original nine-attempt store budget")
	require.Equal(t, 1, creates, "persistence retries must retain the same atom")
	require.True(t, engine.waited, "the original atom must still be monitored")
	require.Equal(t, 1, sink.succeeded)
	require.Zero(t, sink.failed, "write contention is not a task outcome")
	var row models.TaskRun
	require.NoError(t, db.First(&row, "id = ?", taskRun.ID).Error)
	require.Equal(t, "runtime", row.RuntimeID)
	require.Equal(t, taskRun.Attempt, row.Attempt)
}

func TestRuntimeExecutorStartPersistenceStopsUntrackedAtom(t *testing.T) {
	for _, scenario := range []string{"claim-context-cancelled", "claim-context-cause", "task-deadline", "run-deadline", "retention-expired", "permanent-write-error"} {
		t.Run(scenario, func(t *testing.T) {
			store, db, taskRun := seedClaimedTaskRun(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if scenario == "claim-context-cause" {
				var cancelCause context.CancelCauseFunc
				ctx, cancelCause = context.WithCancelCause(ctx)
				cancel = func() { cancelCause(errors.New("claim no longer held")) }
				defer cancel()
			}
			if scenario == "retention-expired" {
				var expire context.CancelFunc
				ctx, expire = context.WithTimeoutCause(ctx, 150*time.Millisecond, errTaskStartNotPersisted)
				defer expire()
			}
			writes := 0
			injectStartWriteError(t, db, func() error {
				writes++
				if scenario == "claim-context-cancelled" || scenario == "claim-context-cause" {
					cancel()
				}
				if scenario == "permanent-write-error" {
					return errors.New("start write rejected")
				}
				return errors.New("database is locked")
			})
			if scenario == "run-deadline" {
				var jobRun models.JobRun
				require.NoError(t, db.First(&jobRun, "id = ?", taskRun.JobRunID).Error)
				setWorkerRunDeadline(t, db, &jobRun, taskRun, 150*time.Millisecond, time.Now())
			}
			engine := &startPersistenceEngine{}
			sink := &fakeSink{}
			executor := &runtimeExecutor{
				store: store, localSink: sink,
				engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) { return engine, nil },
			}
			if scenario == "task-deadline" {
				executor.taskTimeout = 150 * time.Millisecond
			}
			started := time.Now()
			executor.Execute(ctx, taskRun)

			require.Positive(t, writes)
			require.Less(t, time.Since(started), time.Second, "cancellation must reach the start transaction and retry waits")
			require.True(t, engine.stopped, "every failed start write must stop the exact untracked atom")
			require.Equal(t, 1, engine.stopCalls)
			require.Equal(t, "runtime", engine.stopRequest.ID)
			require.True(t, engine.stopRequest.Force)
			require.False(t, engine.waited)
			require.Zero(t, sink.succeeded)
			if scenario == "task-deadline" || scenario == "permanent-write-error" {
				require.Equal(t, 1, sink.failed)
			} else {
				require.Zero(t, sink.failed, "lost claims and run deadlines must not become task failures")
			}
			var row models.TaskRun
			require.NoError(t, db.First(&row, "id = ?", taskRun.ID).Error)
			require.Empty(t, row.RuntimeID)
			require.Equal(t, string(run.TaskStatusRunning), row.Status)
		})
	}
}

func TestRuntimeExecutorStartPersistenceHonorsClaimLossAfterContention(t *testing.T) {
	store, db, taskRun := seedClaimedTaskRun(t)
	contended := make(chan struct{})
	var once sync.Once
	var lost atomic.Bool
	injectStartWriteError(t, db, func() error {
		once.Do(func() { close(contended) })
		if !lost.Load() {
			return errors.New("database is locked")
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reassigned := make(chan error, 1)
	go func() {
		select {
		case <-contended:
		case <-ctx.Done():
			reassigned <- ctx.Err()
			return
		}
		err := db.WithContext(ctx).Model(&models.TaskRun{}).Where("id = ?", taskRun.ID).
			Update("claimed_by", "replacement-worker").Error
		lost.Store(err == nil)
		reassigned <- err
	}()
	engine := &startPersistenceEngine{}
	sink := &fakeSink{}
	(&runtimeExecutor{
		store: store, localSink: sink,
		engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) { return engine, nil },
	}).Execute(ctx, taskRun)

	require.NoError(t, <-reassigned)
	require.NoError(t, ctx.Err(), "the durable claim fence must end the wait before its timeout")
	require.Equal(t, 1, engine.stopCalls)
	require.Equal(t, "runtime", engine.stopRequest.ID)
	require.True(t, engine.stopRequest.Force)
	require.False(t, engine.waited)
	require.Zero(t, sink.failed)
	require.Zero(t, sink.succeeded)
	var row models.TaskRun
	require.NoError(t, db.First(&row, "id = ?", taskRun.ID).Error)
	require.Equal(t, "replacement-worker", row.ClaimedBy)
	require.Empty(t, row.RuntimeID)
}

type startPersistenceEngine struct {
	cancelOnCreateEngine
	stopCalls   int
	stopRequest *atom.EngineStopRequest
}

func (e *startPersistenceEngine) Stop(req *atom.EngineStopRequest) error {
	e.stopCalls++
	e.stopRequest = req
	return e.cancelOnCreateEngine.Stop(req)
}

func injectStartWriteError(t *testing.T, db *gorm.DB, failure func() error) {
	t.Helper()
	const callback = "test:task_start_write"
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		values, ok := tx.Statement.Dest.(map[string]any)
		if tx.Statement.Table == "task_runs" && ok && values["runtime_id"] == "runtime" {
			if err := failure(); err != nil {
				_ = tx.AddError(err)
			}
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Update().Remove(callback) })
}
