package job

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type resourceLocalEngine struct {
	*fakeEngine
	samples atomic.Int32
}
type resourceLocalAtom struct{ atom.Atom }

func (resourceLocalAtom) ResourceOutcome() atom.ResourceOutcome {
	return atom.ResourceOutcome{OOMKilled: true, MemoryLimitBytes: new(int64(64))}
}
func (resourceLocalAtom) ExitCode() *int { return new(137) }
func (e *resourceLocalEngine) Wait(req *atom.EngineWaitRequest) (atom.Atom, error) {
	a, err := e.fakeEngine.Wait(req)
	if err != nil {
		return a, err
	}
	return resourceLocalAtom{a}, nil
}
func (e *resourceLocalEngine) Stats(*atom.EngineStatsRequest) (atom.ResourceStats, error) {
	e.samples.Add(1)
	return atom.ResourceStats{MemoryBytes: new(int64(16)), CPUSeconds: new(1.5)}, nil
}

func TestLocalResourceStatsReachesPersistedRunAndGateOffIsInert(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			t.Cleanup(func() { require.NoError(t, env.Process()) })
			t.Setenv("CAESIUM_RESOURCE_STATS_ENABLED", strconv.FormatBool(enabled))
			require.NoError(t, env.Process())
			db := jobdeftestutil.OpenTestDB(t)
			t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
			store := run.NewStore(db)
			engine := &resourceLocalEngine{fakeEngine: newFakeEngine()}
			jobID, taskID, atomID := uuid.New(), uuid.New(), uuid.New()
			tasks := &fakeTaskService{tasks: models.Tasks{{ID: taskID, JobID: jobID, AtomID: atomID}}}
			persistGraph(t, db, tasks.tasks, nil)
			atoms := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{atomID: fakeModelAtom(atomID)}}
			engine.resultByName[taskID.String()] = atom.Killed
			if enabled {
				engine.resultByName[taskID.String()] = atom.ResourceFailure
			}
			engine.runDurationByName[taskID.String()] = 50 * time.Millisecond
			opts := withTestDeps(store, env.Environment{MaxParallelTasks: 1, TaskFailurePolicy: taskFailurePolicyHalt, ExecutionMode: executionModeLocal, ResourceStatsEnabled: enabled, ResourceStatsSampleInterval: time.Millisecond}, tasks, atoms, &fakeTaskEdgeService{}, engine)
			require.Error(t, New(&models.Job{ID: jobID}, opts...).Run(context.Background()))
			snapshot := latestRunSnapshot(t, store, jobID)
			require.Len(t, snapshot.Tasks, 1)
			row := snapshot.Tasks[0]
			require.Equal(t, 137, *row.ExitCode)
			if enabled {
				require.Greater(t, engine.samples.Load(), int32(0))
				require.True(t, row.OOMKilled)
				require.Equal(t, int64(64), *row.PeakMemoryBytes)
				require.Equal(t, 1.5, *row.CPUSeconds)
				require.Equal(t, "resource_failure", row.Result)
			} else {
				require.Zero(t, engine.samples.Load())
				require.False(t, row.OOMKilled)
				require.Nil(t, row.PeakMemoryBytes)
				require.Nil(t, row.CPUSeconds)
				require.Empty(t, row.StatsSource)
				require.Equal(t, "killed", row.Result)
			}
		})
	}
}

// First provide a valid sample, then leave a request in flight until the
// executor cancels sampling. Stop records whether that request was joined.
type resourceLocalMonitorErrorEngine struct {
	*fakeEngine
	timeout          bool
	calls            atomic.Int32
	pending          chan struct{}
	joined           chan struct{}
	stoppedAfterJoin bool
}

func (e *resourceLocalMonitorErrorEngine) Create(req *atom.EngineCreateRequest) (atom.Atom, error) {
	a, err := e.fakeEngine.Create(req)
	return resourceLocalAtom{a}, err // pre-Wait evidence must never be used on error
}
func (e *resourceLocalMonitorErrorEngine) Stats(req *atom.EngineStatsRequest) (atom.ResourceStats, error) {
	if e.calls.Add(1) == 1 {
		return atom.ResourceStats{MemoryBytes: new(int64(16)), CPUSeconds: new(1.5)}, nil
	}
	close(e.pending)
	<-req.Context.Done()
	close(e.joined)
	return atom.ResourceStats{}, req.Context.Err()
}
func (e *resourceLocalMonitorErrorEngine) Wait(req *atom.EngineWaitRequest) (atom.Atom, error) {
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
func (e *resourceLocalMonitorErrorEngine) Stop(req *atom.EngineStopRequest) error {
	select {
	case <-e.joined:
		e.stoppedAfterJoin = true
	default:
	}
	return e.fakeEngine.Stop(req)
}
func TestLocalResourceStatsSurviveMonitorErrorsBeforeTeardown(t *testing.T) {
	t.Cleanup(func() { require.NoError(t, env.Process()) })
	t.Setenv("CAESIUM_RESOURCE_STATS_ENABLED", "true")
	require.NoError(t, env.Process())
	for _, timeout := range []bool{false, true} {
		t.Run("timeout="+strconv.FormatBool(timeout), func(t *testing.T) {
			db := jobdeftestutil.OpenTestDB(t)
			t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
			store := run.NewStore(db)
			engine := &resourceLocalMonitorErrorEngine{fakeEngine: newFakeEngine(), timeout: timeout, pending: make(chan struct{}), joined: make(chan struct{})}
			jobID, taskID, atomID := uuid.New(), uuid.New(), uuid.New()
			tasks := &fakeTaskService{tasks: models.Tasks{{ID: taskID, JobID: jobID, AtomID: atomID}}}
			persistGraph(t, db, tasks.tasks, nil)
			atoms := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{atomID: fakeModelAtom(atomID)}}
			opts := withTestDeps(store, env.Environment{MaxParallelTasks: 1, TaskFailurePolicy: taskFailurePolicyHalt, ExecutionMode: executionModeLocal, TaskTimeout: 500 * time.Millisecond, ResourceStatsEnabled: true, ResourceStatsSampleInterval: time.Millisecond}, tasks, atoms, &fakeTaskEdgeService{}, engine)
			err := New(&models.Job{ID: jobID}, opts...).Run(context.Background())
			require.Error(t, err)
			if timeout {
				require.ErrorContains(t, err, "timed out")
			} else {
				require.ErrorContains(t, err, "runtime watch disconnected")
			}
			require.True(t, engine.stoppedAfterJoin, "teardown must follow sampler cancellation and join")
			row := latestRunSnapshot(t, store, jobID).Tasks[0]
			require.Equal(t, "sampled", row.StatsSource)
			require.Equal(t, int64(16), *row.PeakMemoryBytes)
			require.Equal(t, 1.5, *row.CPUSeconds)
			require.Nil(t, row.ExitCode, "failed Wait supplied no terminal exit evidence")
			require.False(t, row.OOMKilled, "pre-Wait inspect must not infer an OOM")
		})
	}
}
