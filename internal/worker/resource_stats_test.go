package worker

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/stretchr/testify/require"
)

type resourceWorkerEngine struct {
	attemptResultEngine
	enabled bool
	samples atomic.Int32
	ready   chan struct{}
	once    sync.Once
}
type resourceWorkerAtom struct{ atom.Atom }

func (resourceWorkerAtom) ResourceOutcome() atom.ResourceOutcome {
	return atom.ResourceOutcome{OOMKilled: true, MemoryLimitBytes: new(int64(64))}
}
func (resourceWorkerAtom) ExitCode() *int { return new(137) }
func (e *resourceWorkerEngine) Stats(*atom.EngineStatsRequest) (atom.ResourceStats, error) {
	e.samples.Add(1)
	e.once.Do(func() { close(e.ready) })
	return atom.ResourceStats{MemoryBytes: new(int64(16)), CPUSeconds: new(2.0)}, nil
}
func (e *resourceWorkerEngine) Wait(req *atom.EngineWaitRequest) (atom.Atom, error) {
	if e.enabled {
		select {
		case <-e.ready:
		case <-req.Context.Done():
			return nil, req.Context.Err()
		}
	}
	a, err := e.attemptResultEngine.Wait(req)
	if err != nil {
		return a, err
	}
	return resourceWorkerAtom{a}, nil
}
func TestWorkerResourceStatsReachesInstanceAndGateOffIsInert(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			t.Cleanup(func() { require.NoError(t, env.Process()) })
			t.Setenv("CAESIUM_RESOURCE_STATS_ENABLED", strconv.FormatBool(enabled))
			require.NoError(t, env.Process())
			f := seedFanOutTaskRun(t, "resource-worker", `{"from":"source","failurePolicy":"continue"}`, pkgtask.Partition{Key: "a"}, 2, false)
			result := atom.Killed
			if enabled {
				result = atom.ResourceFailure
			}
			engine := &resourceWorkerEngine{attemptResultEngine: attemptResultEngine{results: []atom.Result{result}}, enabled: enabled, ready: make(chan struct{})}
			executeWithEngine(f, engine)
			row := reloadInstanceRow(t, f.db, f.taskRun.ID)
			require.Equal(t, 137, *row.ExitCode)
			if enabled {
				require.Greater(t, engine.samples.Load(), int32(0))
				require.True(t, row.OOMKilled)
				require.Equal(t, int64(64), *row.PeakMemoryBytes)
				require.Equal(t, 2.0, *row.CPUSeconds)
				require.Equal(t, "resource_failure", row.Result)
			} else {
				require.Zero(t, engine.samples.Load())
				require.False(t, row.OOMKilled)
				require.Nil(t, row.PeakMemoryBytes)
				require.Nil(t, row.CPUSeconds)
				require.Empty(t, row.StatsSource)
			}
		})
	}
}

type resourceWorkerMonitorErrorEngine struct {
	attemptResultEngine
	timeout          bool
	calls            atomic.Int32
	pending          chan struct{}
	joined           chan struct{}
	stoppedAfterJoin bool
}

func (e *resourceWorkerMonitorErrorEngine) Create(req *atom.EngineCreateRequest) (atom.Atom, error) {
	a, err := e.attemptResultEngine.Create(req)
	return resourceWorkerAtom{a}, err // pre-Wait evidence is not a terminal outcome
}
func (e *resourceWorkerMonitorErrorEngine) Stats(req *atom.EngineStatsRequest) (atom.ResourceStats, error) {
	if e.calls.Add(1) == 1 {
		return atom.ResourceStats{MemoryBytes: new(int64(16)), CPUSeconds: new(2.0)}, nil
	}
	close(e.pending)
	<-req.Context.Done()
	close(e.joined)
	return atom.ResourceStats{}, req.Context.Err()
}
func (e *resourceWorkerMonitorErrorEngine) Wait(req *atom.EngineWaitRequest) (atom.Atom, error) {
	select {
	case <-e.pending:
	case <-req.Context.Done():
		return nil, req.Context.Err()
	}
	if e.timeout {
		<-req.Context.Done()
		return nil, req.Context.Err()
	}
	return nil, errors.New("runtime watch disconnected")
}
func (e *resourceWorkerMonitorErrorEngine) Stop(*atom.EngineStopRequest) error {
	select {
	case <-e.joined:
		e.stoppedAfterJoin = true
	default:
	}
	return nil
}
func TestWorkerResourceStatsSurviveMonitorErrorsBeforeTeardown(t *testing.T) {
	t.Cleanup(func() { require.NoError(t, env.Process()) })
	t.Setenv("CAESIUM_RESOURCE_STATS_ENABLED", "true")
	t.Setenv("CAESIUM_RESOURCE_STATS_SAMPLE_INTERVAL", "1ms")
	require.NoError(t, env.Process())
	for _, timeout := range []bool{false, true} {
		t.Run("timeout="+strconv.FormatBool(timeout), func(t *testing.T) {
			f := seedFanOutTaskRun(t, "resource-monitor-error", `{"from":"source","failurePolicy":"continue"}`, pkgtask.Partition{Key: "a"}, 2, false)
			engine := &resourceWorkerMonitorErrorEngine{timeout: timeout, pending: make(chan struct{}), joined: make(chan struct{})}
			executor := &runtimeExecutor{
				store: f.store, localSink: NewLocalSink(f.store), taskTimeout: 500 * time.Millisecond,
				engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) { return engine, nil },
			}
			executor.Execute(context.Background(), f.taskRun)
			require.True(t, engine.stoppedAfterJoin, "teardown must follow sampler cancellation and join")
			row := reloadInstanceRow(t, f.db, f.taskRun.ID)
			require.Equal(t, "failed", row.Status)
			if timeout {
				// Either select door can observe the expired task deadline.
				require.NotEmpty(t, row.Error)
			} else {
				require.Contains(t, row.Error, "runtime watch disconnected")
			}
			require.Equal(t, "sampled", row.StatsSource)
			require.Equal(t, int64(16), *row.PeakMemoryBytes)
			require.Equal(t, 2.0, *row.CPUSeconds)
			require.Nil(t, row.ExitCode, "failed Wait supplied no terminal exit evidence")
			require.False(t, row.OOMKilled, "pre-Wait inspect must not infer an OOM")
		})
	}
}

func TestWorkerResourceStatsReclaimClearsOnlyExpiredRuntimeEvidence(t *testing.T) {
	t.Cleanup(func() { require.NoError(t, env.Process()) })
	t.Setenv("CAESIUM_RESOURCE_STATS_ENABLED", "true")
	require.NoError(t, env.Process())
	for _, liveOwner := range []bool{false, true} {
		t.Run("live-owner="+strconv.FormatBool(liveOwner), func(t *testing.T) {
			db := jobdeftestutil.OpenTestDB(t)
			t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
			store := run.NewStore(db)
			jr := seedJobRun(t, db, string(run.StatusRunning))
			if liveOwner {
				seedRunLease(t, db, jr, "live-owner", time.Now().Add(time.Minute))
			}
			row := *seedTaskRun(t, db, seedTaskRunInput{jobRunID: &jr, status: "running", claimedBy: "old-worker", claimAttempt: 2, claimExpiresAt: new(time.Now().Add(-time.Minute)), createdAt: time.Now().Add(-time.Hour)})
			require.NoError(t, store.StartTaskClaimed(jr, row.ID, "lost-runtime", "old-worker"))
			row = reloadInstanceRow(t, db, row.ID)
			old := run.TaskResourceOutcome{RuntimeID: row.RuntimeID, Attempt: row.Attempt, ClaimedBy: row.ClaimedBy, ClaimAttempt: row.ClaimAttempt, ExitCode: new(137), ResourceSummary: atom.ResourceSummary{PeakMemoryBytes: new(int64(64)), CPUSeconds: new(2.0), StatsSource: "oom_inferred", OOMKilled: true}}
			require.NoError(t, store.SetTaskResourceOutcome(jr, row.ID, old))
			require.NoError(t, db.Model(&row).Updates(map[string]any{"applied_resources": `{"memoryBytes":64}`, "escalation_level": 2}).Error)
			claimer := NewClaimer("new-worker", store, time.Minute)
			require.NoError(t, claimer.ReclaimExpired(context.Background()))
			reset := reloadInstanceRow(t, db, row.ID)
			if liveOwner {
				require.Equal(t, "running", reset.Status)
				require.True(t, reset.OOMKilled)
				require.Equal(t, "oom_inferred", reset.StatsSource, "live-owner guard protects resource observations too")
				return
			}
			require.Equal(t, "pending", reset.Status)
			require.Nil(t, reset.PeakMemoryBytes)
			require.Nil(t, reset.CPUSeconds)
			require.Nil(t, reset.ExitCode)
			require.False(t, reset.OOMKilled)
			require.Empty(t, reset.StatsSource)
			require.Empty(t, reset.AppliedResources)
			require.Zero(t, reset.EscalationLevel)
			claimed, err := claimer.ClaimNext(context.Background())
			require.NoError(t, err)
			require.NotNil(t, claimed)
			require.Equal(t, row.ID, claimed.ID)
			require.NoError(t, store.StartTaskClaimed(jr, row.ID, "replacement", "new-worker"))
			require.NoError(t, store.SetTaskResourceOutcome(jr, row.ID, old))
			reset = reloadInstanceRow(t, db, row.ID)
			require.Empty(t, reset.StatsSource, "lost runtime cannot write after re-claim")
			next := run.TaskResourceOutcome{RuntimeID: "replacement", Attempt: claimed.Attempt, ClaimedBy: claimed.ClaimedBy, ClaimAttempt: claimed.ClaimAttempt, ExitCode: new(0), ResourceSummary: atom.ResourceSummary{PeakMemoryBytes: new(int64(16)), StatsSource: "sampled"}}
			require.NoError(t, store.SetTaskResourceOutcome(jr, row.ID, next))
			reset = reloadInstanceRow(t, db, row.ID)
			require.Equal(t, "sampled", reset.StatsSource)
			require.Equal(t, int64(16), *reset.PeakMemoryBytes)
			require.Equal(t, 0, *reset.ExitCode)
			require.False(t, reset.OOMKilled)
		})
	}
}
