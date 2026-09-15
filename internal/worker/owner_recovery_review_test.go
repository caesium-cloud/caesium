package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/dispatch"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestClaimerMemoryModeReservesExpiredLeaseForTakeover(t *testing.T) {
	for _, memory := range []bool{false, true} {
		t.Run(fmt.Sprint(memory), func(t *testing.T) {
			t.Cleanup(func() { require.NoError(t, env.Process()) })
			t.Setenv("CAESIUM_RUN_OWNER_ENABLED", "true")
			t.Setenv("CAESIUM_RUN_OWNER_IN_MEMORY", fmt.Sprint(memory))
			t.Setenv("CAESIUM_EXECUTION_MODE", " Distributed ")
			require.NoError(t, env.Process())
			db := jobdeftestutil.OpenTestDB(t)
			t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
			now := time.Now().UTC()
			runID := seedJobRun(t, db, string(run.StatusRunning))
			seedRunLease(t, db, runID, "dead-owner", now.Add(-time.Hour))
			ready := seedTaskRun(t, db, seedTaskRunInput{status: string(run.TaskStatusPending), jobRunID: &runID})
			store := run.NewStore(db)
			claimer := NewClaimer("pull-worker", store, time.Minute)
			// Configuration changes after construction cannot weaken the stored mode.
			t.Setenv("CAESIUM_RUN_OWNER_IN_MEMORY", fmt.Sprint(!memory))
			require.NoError(t, env.Process())
			claimed, err := claimer.ClaimNext(t.Context())
			require.NoError(t, err)
			if memory {
				require.Nil(t, claimed)
			} else {
				require.Equal(t, ready.ID, claimed.ID)
			}
			// An expired task claim is also exclusively recovered by the memory owner.
			require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", ready.ID).Updates(map[string]any{"status": "running", "claimed_by": "lost-worker", "claim_expires_at": now.Add(-time.Minute)}).Error)
			require.NoError(t, claimer.ReclaimExpired(t.Context()))
			var row models.TaskRun
			require.NoError(t, db.First(&row, "id = ?", ready.ID).Error)
			if memory {
				require.Equal(t, "running", row.Status)
				require.Equal(t, "lost-worker", row.ClaimedBy)
				// Removing the reservation restores the SQL lane; memory mode
				// must not block a run which has never acquired an owner lease.
				require.NoError(t, db.Where("run_id = ?", runID.String()).Delete(&models.RunLease{}).Error)
				require.NoError(t, claimer.ReclaimExpired(t.Context()))
				claimed, err = claimer.ClaimNext(t.Context())
				require.NoError(t, err)
				require.NotNil(t, claimed)
				require.Equal(t, ready.ID, claimed.ID)
			} else {
				require.Equal(t, "pending", row.Status)
				require.Empty(t, row.ClaimedBy)
			}
		})
	}
}

func TestOwnerSinkRecoveryOutlastsContentionBudgetWithoutChangingResult(t *testing.T) {
	// Real recovery lasts longer than the former 1.55-second contention budget.
	started := time.Now()
	var requests []dispatch.CompleteRequest
	sink := newOwnerSink(ownerMeta(), func(_ context.Context, _, _ string, req dispatch.CompleteRequest) (*dispatch.CompleteResponse, error) {
		requests = append(requests, req)
		if time.Since(started) < 2*time.Second {
			return nil, dispatch.ErrOwnerNotReady
		}
		return &dispatch.CompleteResponse{Accepted: true}, nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, sink.Succeeded(ctx, sampleTaskRun(), "success", map[string]string{"business": "value"}, []string{"next"}))
	require.Greater(t, len(requests), 1)
	for _, req := range requests[1:] {
		require.Equal(t, requests[0], req)
	}
}

func TestOwnerSinkRecoveryStopsWhenClaimContextIsLost(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	claimLost := errors.New("claim no longer held")
	calls := 0
	sink := newOwnerSink(ownerMeta(), func(context.Context, string, string, dispatch.CompleteRequest) (*dispatch.CompleteResponse, error) {
		calls++
		cancel(claimLost)
		return nil, dispatch.ErrOwnerNotReady
	})
	err := sink.Succeeded(ctx, sampleTaskRun(), "success", nil, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, claimLost, context.Cause(ctx))
	require.Equal(t, 1, calls)
}

func TestRuntimeExecutorRecoveryContextDeadlineDoesNotRetryFinishedAtom(t *testing.T) {
	f := seedProducerTaskRun(t, "recovery-parent-deadline")
	engine := &captureCreateEngine{logs: "done\n", waitResult: atom.Success}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	posts := 0
	executor := &runtimeExecutor{store: f.store, localSink: &fakeSink{}, engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) { return engine, nil }, completePost: func(ctx context.Context, _ string, _ string, _ dispatch.CompleteRequest) (*dispatch.CompleteResponse, error) {
		posts++
		<-ctx.Done()
		return nil, dispatch.ErrOwnerNotReady
	}}
	executor.Execute(withDispatchMeta(ctx, ownerMeta()), f.taskRun)
	require.Equal(t, 1, posts)
	require.NotNil(t, engine.createReq)
	var row models.TaskRun
	require.NoError(t, f.db.First(&row, "id = ?", f.taskRun.ID).Error)
	require.NotEqual(t, "failed", row.Status, "lost claim context must not create an ordinary failure")
}

func TestRuntimeExecutorExpiredDeadlineLogsFinalizerEvidence(t *testing.T) {
	f := seedProducerTaskRun(t, "deadline-reason-evidence")
	started := time.Now().Add(-time.Hour)
	setWorkerRunDeadline(t, f.db, f.jobRun, f.taskRun, time.Second, started)
	core, entries := observer.New(zap.WarnLevel)
	restore := zap.ReplaceGlobals(zap.New(core))
	defer restore()
	sink := &fakeSink{}
	(&runtimeExecutor{store: f.store, localSink: sink}).Execute(t.Context(), f.taskRun)
	records := entries.FilterMessage("worker task awaiting atomic run deadline finalization").All()
	require.Len(t, records, 1)
	fields := records[0].ContextMap()
	require.Equal(t, "run_owner_finalization_pending", fields["reason"])
	require.Equal(t, "before_execution", fields["stage"])
	require.Equal(t, f.taskRun.ID.String(), fmt.Sprint(fields["task_run_id"]))
	require.Equal(t, f.taskRun.JobRunID.String(), fmt.Sprint(fields["run_id"]))
	require.Contains(t, fields, "deadline")
	require.Contains(t, fields, "observed_at")
	require.Zero(t, sink.failed)
	var row models.TaskRun
	require.NoError(t, f.db.First(&row, "id = ?", f.taskRun.ID).Error)
	require.Equal(t, "running", row.Status, "owner must perform the atomic run-and-task timeout transition")
	require.Equal(t, f.taskRun.ClaimedBy, row.ClaimedBy)
	require.Zero(t, row.TerminalSequence)
	// Prove deterministic boundary and all-stage evidence through the same helper.
	timeout := executionTimeouts{runDeadline: started.Add(time.Second), runTimeout: time.Second}
	require.False(t, (&runtimeExecutor{}).deferRunDeadline(f.taskRun, timeout, "cache_completion", timeout.runDeadline.Add(-time.Nanosecond)))
	require.True(t, (&runtimeExecutor{}).deferRunDeadline(f.taskRun, timeout, "cache_completion", timeout.runDeadline))
	require.True(t, (&runtimeExecutor{}).deferRunDeadline(f.taskRun, timeout, "after_attempt", timeout.runDeadline.Add(time.Nanosecond)))
}

func TestRunCompletionContextPreservesRunWindowAndParent(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	deadline := time.Now().Add(time.Hour)
	timeouts := executionTimeouts{taskTimeout: time.Nanosecond, runTimeout: time.Hour, runDeadline: deadline}
	ctx, stop := runCompletionContext(parent, timeouts)
	defer stop()
	observed, ok := ctx.Deadline()
	require.True(t, ok)
	require.Equal(t, deadline, observed)
	require.NoError(t, ctx.Err(), "finished atom's per-task duration must not cut off completion reporting")
	cancel()
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	expired, stopExpired := runCompletionContext(t.Context(), executionTimeouts{runTimeout: time.Second, runDeadline: time.Now().Add(-time.Second)})
	defer stopExpired()
	require.ErrorIs(t, expired.Err(), context.DeadlineExceeded)
	require.True(t, run.IsRunDeadlineError(context.Cause(expired)))
}

type reviewCountingEngine struct {
	*captureCreateEngine
	creates int
}

func (e *reviewCountingEngine) Create(req *atom.EngineCreateRequest) (atom.Atom, error) {
	e.creates++
	return e.captureCreateEngine.Create(req)
}

func TestRuntimeExecutorRecoveryFenceDoesNotReplayFinishedAtom(t *testing.T) {
	f := seedProducerTaskRun(t, "recovery-fence-no-replay")
	f.taskRun.MaxAttempts = 3
	engine := &reviewCountingEngine{captureCreateEngine: &captureCreateEngine{logs: "done\n", waitResult: atom.Success}}
	posts := 0
	(&runtimeExecutor{store: f.store, localSink: &fakeSink{}, engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) { return engine, nil }, completePost: func(context.Context, string, string, dispatch.CompleteRequest) (*dispatch.CompleteResponse, error) {
		posts++
		return nil, dispatch.ErrOwnerRejected
	}}).Execute(withDispatchMeta(t.Context(), ownerMeta()), f.taskRun)
	require.Equal(t, 1, posts, "an ownership fence cannot become another task attempt")
	require.Equal(t, 1, engine.creates)
	var row models.TaskRun
	require.NoError(t, f.db.First(&row, "id = ?", f.taskRun.ID).Error)
	require.NotEqual(t, "failed", row.Status)
}

func TestRuntimeExecutorRecoveryReportStopsAtRunDeadline(t *testing.T) {
	f := seedProducerTaskRun(t, "recovery-report-run-window")
	setWorkerRunDeadline(t, f.db, f.jobRun, f.taskRun, time.Second, time.Now())
	f.taskRun.MaxAttempts = 3
	engine := &reviewCountingEngine{captureCreateEngine: &captureCreateEngine{logs: "done\n", waitResult: atom.Success}}
	var statuses []string
	parent := t.Context()
	(&runtimeExecutor{store: f.store, localSink: &fakeSink{}, engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) { return engine, nil }, completePost: func(_ context.Context, _ string, _ string, req dispatch.CompleteRequest) (*dispatch.CompleteResponse, error) {
		statuses = append(statuses, req.Status)
		return nil, dispatch.ErrOwnerNotReady
	}}).Execute(withDispatchMeta(parent, ownerMeta()), f.taskRun)
	require.NoError(t, parent.Err(), "only absolute run budget expired; worker parent is still live")
	require.NotEmpty(t, statuses)
	for _, status := range statuses {
		require.Equal(t, "succeeded", status)
	}
	require.Equal(t, 1, engine.creates)
	var row models.TaskRun
	require.NoError(t, f.db.First(&row, "id = ?", f.taskRun.ID).Error)
	require.NotEqual(t, "failed", row.Status, "run owner retains the atomic all-task timeout transition")
	require.NotEqual(t, "succeeded", row.Status)
}

func TestRuntimeExecutorCacheRecoveryAbortNeverStartsAtom(t *testing.T) {
	for _, runDeadline := range []bool{false, true} {
		t.Run(fmt.Sprint(runDeadline), func(t *testing.T) {
			f := seedProducerTaskRun(t, "cache-recovery-abort")
			cold := &partitionEmittingEngine{logs: producerMarkerLog}
			(&runtimeExecutor{store: f.store, localSink: NewLocalSink(f.store), engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) { return cold, nil }}).Execute(t.Context(), f.taskRun)
			fresh := f.newProducerRunAttempt(t)
			if runDeadline {
				setWorkerRunDeadline(t, f.db, f.jobRun, fresh, time.Second, time.Now())
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			engine := &reviewCountingEngine{captureCreateEngine: &captureCreateEngine{}}
			var statuses []string
			(&runtimeExecutor{store: f.store, localSink: &fakeSink{}, engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) { return engine, nil }, completePost: func(_ context.Context, _ string, _ string, req dispatch.CompleteRequest) (*dispatch.CompleteResponse, error) {
				statuses = append(statuses, req.Status)
				if !runDeadline {
					cancel()
				}
				return nil, dispatch.ErrOwnerNotReady
			}}).Execute(withDispatchMeta(ctx, ownerMeta()), fresh)
			require.NotEmpty(t, statuses, "must reach a real cache completion report")
			for _, status := range statuses {
				require.Equal(t, "cached", status, "canceled cache report cannot become runtime execution or failure")
			}
			require.Zero(t, engine.creates)
			var row models.TaskRun
			require.NoError(t, f.db.First(&row, "id = ?", fresh.ID).Error)
			require.NotEqual(t, "cached", row.Status)
			require.NotEqual(t, "failed", row.Status)
			if runDeadline {
				require.NoError(t, ctx.Err())
			}
		})
	}
}

func TestRuntimeExecutorFailureRecoveryStopsAtRunDeadline(t *testing.T) {
	f := seedProducerTaskRun(t, "failure-recovery-run-window")
	setWorkerRunDeadline(t, f.db, f.jobRun, f.taskRun, time.Second, time.Now())
	engine := &reviewCountingEngine{captureCreateEngine: &captureCreateEngine{logs: "##caesium::partitions not-json\n", waitResult: atom.Success}}
	var statuses []string
	parent := t.Context()
	(&runtimeExecutor{store: f.store, localSink: &fakeSink{}, engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) { return engine, nil }, completePost: func(_ context.Context, _ string, _ string, req dispatch.CompleteRequest) (*dispatch.CompleteResponse, error) {
		statuses = append(statuses, req.Status)
		return nil, dispatch.ErrOwnerNotReady
	}}).Execute(withDispatchMeta(parent, ownerMeta()), f.taskRun)
	require.NoError(t, parent.Err())
	require.NotEmpty(t, statuses)
	for _, status := range statuses {
		require.Equal(t, "failed", status)
	}
	require.Equal(t, 1, engine.creates)
	var row models.TaskRun
	require.NoError(t, f.db.First(&row, "id = ?", f.taskRun.ID).Error)
	require.NotEqual(t, "failed", row.Status, "must leave atomic all-task timeout to run owner")
	var runRow models.JobRun
	require.NoError(t, f.db.First(&runRow, "id = ?", f.taskRun.JobRunID).Error)
	require.Equal(t, "running", runRow.Status, "ordinary failure halt must not replace the typed run-timeout transition")
}
