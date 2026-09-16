package job

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// createAtDeadlineEngine models both a context-aware backend that refuses to
// start after the attempt deadline and an injected backend that returns an atom
// despite cancellation. The latter must be stopped before StartTask persists it.
type createAtDeadlineEngine struct {
	atom.Engine
	ctx           context.Context
	ignoreContext bool
	launched      *atomic.Bool
}

func (e *createAtDeadlineEngine) Create(req *atom.EngineCreateRequest) (atom.Atom, error) {
	<-e.ctx.Done()
	if !e.ignoreContext {
		return nil, e.ctx.Err()
	}
	e.launched.Store(true)
	return e.Engine.Create(req)
}

func TestRunLocalCreateUsesAttemptDeadlineAndCleansLateAtom(t *testing.T) {
	for _, tt := range []struct {
		name          string
		ignoreContext bool
	}{
		{name: "context-aware create"},
		{name: "late atom returned after deadline", ignoreContext: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := jobdeftestutil.OpenTestDB(t)
			t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
			store := run.NewStore(db)
			base := newFakeEngine()
			jobID, taskID, atomID := uuid.New(), uuid.New(), uuid.New()
			taskSvc := &fakeTaskService{tasks: models.Tasks{{ID: taskID, JobID: jobID, AtomID: atomID}}}
			persistGraph(t, db, taskSvc.tasks, nil)
			atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{atomID: fakeModelAtom(atomID)}}
			const taskTimeout = 500 * time.Millisecond
			var launched atomic.Bool
			var factoryCtx context.Context
			opts := withTestDeps(store, env.Environment{
				MaxParallelTasks:  1,
				TaskFailurePolicy: taskFailurePolicyHalt,
				ExecutionMode:     executionModeLocal,
				TaskTimeout:       taskTimeout,
			}, taskSvc, atomSvc, &fakeTaskEdgeService{}, base)
			opts = append(opts, WithDockerEngineFactory(func(ctx context.Context) atom.Engine {
				factoryCtx = ctx
				return &createAtDeadlineEngine{
					Engine:        base,
					ctx:           ctx,
					ignoreContext: tt.ignoreContext,
					launched:      &launched,
				}
			}))

			err := New(&models.Job{ID: jobID, TaskTimeout: taskTimeout, RunTimeout: 2 * time.Second}, opts...).Run(context.Background())
			require.ErrorContains(t, err, "timed out after 500ms")
			require.NotContains(t, err.Error(), "run timed out")
			require.NotNil(t, factoryCtx, "the attempt must construct its own engine")
			deadline, ok := factoryCtx.Deadline()
			require.True(t, ok, "the engine must receive the task deadline")
			require.LessOrEqual(t, time.Until(deadline), time.Duration(0), "the task deadline must have elapsed")
			require.Equal(t, tt.ignoreContext, launched.Load())

			snapshot := latestRunSnapshot(t, store, jobID)
			require.Equal(t, run.TaskStatusFailed, taskRunByID(snapshot, taskID).Status)
			require.Empty(t, taskRunByID(snapshot, taskID).RuntimeID,
				"a late Create result must not be persisted as a started task")
			if tt.ignoreContext {
				require.True(t, base.wasForceStopped(taskID.String()), "the late atom must be removed")
			}
		})
	}
}

func TestRunLocalKubernetesFactoryErrorFailsTask(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
	store := run.NewStore(db)
	jobID, taskID, atomID := uuid.New(), uuid.New(), uuid.New()
	taskSvc := &fakeTaskService{tasks: models.Tasks{{ID: taskID, JobID: jobID, AtomID: atomID}}}
	persistGraph(t, db, taskSvc.tasks, nil)
	modelAtom := fakeModelAtom(atomID)
	modelAtom.Engine = models.AtomEngineKubernetes
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{atomID: modelAtom}}
	wantErr := errors.New("kubernetes engine unavailable: no kubeconfig")
	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  1,
		TaskFailurePolicy: taskFailurePolicyHalt,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, &fakeTaskEdgeService{}, newFakeEngine())
	opts = append(opts, WithKubernetesEngineFactory(func(context.Context) (atom.Engine, error) {
		return nil, wantErr
	}))

	err := New(&models.Job{ID: jobID}, opts...).Run(context.Background())
	require.ErrorIs(t, err, wantErr)
	require.ErrorContains(t, err, taskID.String())
	snapshot := latestRunSnapshot(t, store, jobID)
	require.Equal(t, run.TaskStatusFailed, taskRunByID(snapshot, taskID).Status)
}

func TestRunLocalFanoutRetryGetsDistinctAttemptContexts(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c"]`, &schema.FanOut{From: "list", MaxPartitions: 16}, 1)
	f.engine.failCreateTimes["b"] = 1
	vars := defaultFanOutVars()
	vars.TaskTimeout = time.Second
	opts := withTestDeps(f.store, vars, f.taskSvc, f.atomSvc, f.edgeSvc, f.engine)
	var mu sync.Mutex
	var contexts []context.Context
	opts = append(opts, WithDockerEngineFactory(func(ctx context.Context) atom.Engine {
		mu.Lock()
		contexts = append(contexts, ctx)
		mu.Unlock()
		return f.engine
	}))

	require.NoError(t, New(&models.Job{ID: f.jobID, TaskTimeout: time.Second}, opts...).Run(context.Background()))
	require.Equal(t, 2, f.engine.createCount("b"), "the partition should retry once")
	mu.Lock()
	got := append([]context.Context(nil), contexts...)
	mu.Unlock()
	require.Len(t, got, 5, "producer, three partitions, and the retried partition each need an engine")
	seen := make(map[context.Context]bool, len(got))
	for _, ctx := range got {
		_, hasDeadline := ctx.Deadline()
		require.True(t, hasDeadline)
		require.False(t, seen[ctx], "fan-out siblings and retries must not share a context")
		require.ErrorIs(t, ctx.Err(), context.Canceled, "finished attempt contexts should be canceled")
		seen[ctx] = true
	}
}
