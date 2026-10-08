package job

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The subscription is the real distributed waiter handoff: registration and
// its durable task reads have finished when the owner begins observing events.
type cancellationWaitBus struct {
	event.Bus
	ready chan uuid.UUID
}

func (b *cancellationWaitBus) Subscribe(ctx context.Context, filter event.Filter) (<-chan event.Event, error) {
	ch, err := b.Bus.Subscribe(ctx, filter)
	if err == nil && filter.RunID != uuid.Nil {
		select {
		case b.ready <- filter.RunID:
		default:
		}
	}
	return ch, err
}

func TestDistributedOwnerCancellationMarksRealCompletion(t *testing.T) {
	for _, cause := range []error{context.Canceled, errors.New("owned server shutdown")} {
		t.Run(cause.Error(), func(t *testing.T) {
			db := jobdeftestutil.OpenTestDB(t)
			t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
			store := run.NewStore(db)
			bus := &cancellationWaitBus{Bus: event.New(), ready: make(chan uuid.UUID, 1)}
			store.SetBus(bus)
			jobID, taskID, atomID := uuid.New(), uuid.New(), uuid.New()
			model := &models.Job{ID: jobID, Alias: "distributed-owner-cancel", RunTimeout: time.Hour}
			require.NoError(t, db.Create(model).Error)
			tasks := &fakeTaskService{tasks: models.Tasks{{ID: taskID, JobID: jobID, AtomID: atomID}}}
			persistGraph(t, db, tasks.tasks, nil)
			atoms := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{atomID: fakeModelAtom(atomID)}}
			engine := newFakeEngine()
			opts := withTestDeps(store, env.Environment{ExecutionMode: executionModeDistributed, WorkerPollInterval: time.Hour},
				tasks, atoms, &fakeTaskEdgeService{}, engine)
			ownerCtx, cancel := context.WithCancelCause(t.Context())
			done := make(chan error, 1)
			go func() { done <- New(model, opts...).Run(ownerCtx) }()
			t.Cleanup(func() {
				cancel(nil)
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("distributed owner did not join after cancellation")
				}
			})
			var runID uuid.UUID
			select {
			case runID = <-bus.ready:
			case <-time.After(10 * time.Second):
				t.Fatal("job never reached its actual distributed waiter")
			}
			var row models.TaskRun
			require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&row).Error)
			require.NoError(t, db.Model(&row).Updates(map[string]any{"status": string(run.TaskStatusRunning), "claimed_by": "worker", "runtime_id": "owned-runtime"}).Error)
			cancel(cause)
			var returned error
			select {
			case returned = <-done:
				// Keep cleanup's join observation available without starting work.
				done <- returned
			case <-time.After(10 * time.Second):
				t.Fatal("canceled distributed job did not return")
			}
			require.True(t, run.IsRunCancellationError(returned))
			require.ErrorIs(t, returned, cause)
			require.Equal(t, cause.Error(), returned.Error())
			snapshot, err := store.Get(runID)
			require.NoError(t, err)
			require.Equal(t, run.StatusFailed, snapshot.Status)
			require.Equal(t, cause.Error(), snapshot.Error)
			require.NoError(t, db.First(&row, "id = ?", row.ID).Error)
			require.Equal(t, string(run.TaskStatusFailed), row.Status)
			require.Equal(t, cause.Error(), row.Error)
			require.NotNil(t, row.CompletedAt)
			require.Empty(t, row.ClaimedBy)
			require.Empty(t, engine.createRequestsForTask(taskID), "distributed waiter must not execute a local atom")
		})
	}
}

func TestRunDeadlineCleanupDoesNotMarkSuccessfulOwnerCancelled(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
	store := run.NewStore(db)
	jobID, taskID, atomID := uuid.New(), uuid.New(), uuid.New()
	model := &models.Job{ID: jobID, Alias: "successful-owner-deadline", RunTimeout: time.Hour}
	require.NoError(t, db.Create(model).Error)
	tasks := &fakeTaskService{tasks: models.Tasks{{ID: taskID, JobID: jobID, AtomID: atomID}}}
	persistGraph(t, db, tasks.tasks, nil)
	atoms := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{atomID: fakeModelAtom(atomID)}}
	opts := withTestDeps(store, env.Environment{ExecutionMode: executionModeLocal, MaxParallelTasks: 1},
		tasks, atoms, &fakeTaskEdgeService{}, newFakeEngine())
	require.NoError(t, New(model, opts...).Run(t.Context()))
	snapshot := latestRunSnapshot(t, store, jobID)
	require.Equal(t, run.StatusSucceeded, snapshot.Status)
	require.Empty(t, snapshot.Error)
	require.Equal(t, run.TaskStatusSucceeded, taskStatusByID(snapshot)[taskID])
}

func TestBackendCancelledErrorDoesNotMarkLiveOwnerCancelled(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
	store := run.NewStore(db)
	jobID, taskID, atomID := uuid.New(), uuid.New(), uuid.New()
	model := &models.Job{ID: jobID, Alias: "backend-canceled-live-owner", RunTimeout: time.Hour}
	require.NoError(t, db.Create(model).Error)
	tasks := &fakeTaskService{tasks: models.Tasks{{ID: taskID, JobID: jobID, AtomID: atomID}}}
	persistGraph(t, db, tasks.tasks, nil)
	atoms := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{atomID: fakeModelAtom(atomID)}}
	engine := newFakeEngine()
	engine.createErrByName[taskID.String()] = context.Canceled
	opts := withTestDeps(store, env.Environment{ExecutionMode: executionModeLocal, MaxParallelTasks: 1},
		tasks, atoms, &fakeTaskEdgeService{}, engine)
	ownerCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	returned := New(model, opts...).Run(ownerCtx)
	require.ErrorIs(t, returned, context.Canceled)
	require.False(t, run.IsRunCancellationError(returned))
	require.NoError(t, ownerCtx.Err())
}
