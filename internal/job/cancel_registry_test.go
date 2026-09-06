package job

import (
	"context"
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

func TestCancelRegistryCancelsEveryContextForOneRun(t *testing.T) {
	r := newCancelRegistry()
	runA, runB := uuid.New(), uuid.New()

	// Two engines against the same run: a partition retry starts a replacement
	// while the previous engine is still draining. A cancel must reach both.
	ctx1, release1 := r.register(context.Background(), runA)
	ctx2, release2 := r.register(context.Background(), runA)
	other, releaseOther := r.register(context.Background(), runB)
	defer release1()
	defer release2()
	defer releaseOther()

	require.Equal(t, 2, r.tracked(runA))
	require.Equal(t, 2, r.cancel(runA))

	require.ErrorIs(t, ctx1.Err(), context.Canceled)
	require.ErrorIs(t, ctx2.Err(), context.Canceled)
	require.NoError(t, other.Err(), "a cancel must not reach another run's context")
}

func TestCancelRegistryReleaseDropsAndIsIdempotent(t *testing.T) {
	r := newCancelRegistry()
	runID := uuid.New()

	ctx, release := r.register(context.Background(), runID)
	require.Equal(t, 1, r.tracked(runID))

	release()
	release()

	require.Zero(t, r.tracked(runID), "release must drop the entry so the map cannot grow unbounded")
	require.ErrorIs(t, ctx.Err(), context.Canceled, "release must cancel its own context (go vet lostcancel)")
	require.Zero(t, r.cancel(runID), "a released run has nothing left to cancel")
}

func TestCancelRegistryNilRunIDIsANoOp(t *testing.T) {
	r := newCancelRegistry()
	parent := context.Background()
	ctx, release := r.register(parent, uuid.Nil)
	require.Equal(t, parent, ctx)
	release()
	require.Zero(t, r.cancel(uuid.Nil))
}

// TestSubscribeRunCancellationsCancelsOnEvent drives the seam the way
// cmd/start/start.go wires it: run.Store publishes run_cancelled on the
// in-process bus, and the subscriber turns it into a context cancel. This is
// what makes CancelRun and the concurrency `replace` admission — which share
// cancelRunTx and therefore the same event — reach the executor.
func TestSubscribeRunCancellationsCancelsOnEvent(t *testing.T) {
	bus := event.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	SubscribeRunCancellations(ctx, bus)

	runID := uuid.New()
	runCtx, release := RegisterRunCancel(context.Background(), runID)
	defer release()

	bus.Publish(event.Event{Type: event.TypeRunCancelled, RunID: runID, Timestamp: time.Now().UTC()})

	select {
	case <-runCtx.Done():
		require.ErrorIs(t, runCtx.Err(), context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("run_cancelled did not cancel the registered run context")
	}
}

// TestRunLocalCancelStopsAtom is the end of the chain A3 exists for: a run
// cancelled while a container is mid-flight must FORCE-STOP that container, not
// merely write the row cancelled and walk away.
//
// It drives the real wiring — store.CancelRun → run_cancelled on the bus →
// SubscribeRunCancellations → the registered run context → the executor's
// taskCtx.Done() branch → engine.Stop(Force: true) — with a fake engine
// standing in for Docker, and asserts both halves: the atom was force-stopped,
// and the cancelled row was NOT resurrected by the task write that follows.
func TestRunLocalCancelStopsAtom(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	bus := event.New()
	store.SetBus(bus)

	subCtx, stopSub := context.WithCancel(context.Background())
	defer stopSub()
	SubscribeRunCancellations(subCtx, bus)

	engine := newFakeEngine()

	jobID := uuid.New()
	taskID := uuid.New()
	atomID := uuid.New()

	taskSvc := &fakeTaskService{tasks: models.Tasks{
		{ID: taskID, JobID: jobID, AtomID: atomID},
	}}
	persistGraph(t, db, taskSvc.tasks, nil)
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{
		atomID: fakeModelAtom(atomID),
	}}

	// The `sleep 120` of the integration scenario: long enough that the cancel
	// provably lands mid-flight.
	engine.runDurationByName[taskID.String()] = 10 * time.Second

	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "cancel-stops-atom"}).Error)
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  1,
		TaskFailurePolicy: taskFailurePolicyHalt,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, &fakeTaskEdgeService{}, engine)

	runCtx, release := RegisterRunCancel(context.Background(), runRecord.ID)
	defer release()

	done := make(chan error, 1)
	go func() {
		done <- New(&models.Job{ID: jobID}, opts...).Run(run.WithContext(runCtx, runRecord.ID))
	}()

	// Wait for the container to exist before cancelling: cancelling before the
	// atom is created would prove nothing about reaching it.
	require.Eventually(t, func() bool {
		return len(engine.createRequestsForTask(taskID)) > 0
	}, 10*time.Second, 10*time.Millisecond, "the executor never created the atom")

	require.NoError(t, store.CancelRun(context.Background(), runRecord.ID))

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the cancelled run never returned")
	}

	require.True(t, engine.wasForceStopped(taskID.String()),
		"a cancelled run must force-stop its in-flight container, not abandon it")

	var row models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runRecord.ID, taskID).First(&row).Error)
	require.Equal(t, string(run.TaskStatusCancelled), row.Status,
		"the cancelled row must stay cancelled — the post-cancel task write is a no-op against a terminal row")
}
