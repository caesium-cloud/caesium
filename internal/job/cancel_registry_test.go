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
	ctx := t.Context()
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
// The `retries` case is not a variation for completeness — it is its own bug.
// Cancelling attempt 1 makes the attempt FAIL, and a retry budget turns that
// failure into attempt 2: the executor re-entered the loop, retryTask flipped
// the cancelled row back to pending (it had no terminal guard, unlike its
// fanned twin), StartTask's guard then saw a legitimately pending row, and a
// second container started on a run the operator had already cancelled. The
// cancel was what triggered the container it was supposed to prevent.
func TestRunLocalCancelStopsAtom(t *testing.T) {
	for _, tc := range []struct {
		name    string
		retries int
	}{
		{name: "no retries", retries: 0},
		{name: "with a retry budget", retries: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := jobdeftestutil.OpenTestDB(t)
			t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

			store := run.NewStore(db)
			bus := event.New()
			store.SetBus(bus)

			subCtx := t.Context()
			SubscribeRunCancellations(subCtx, bus)

			engine := newFakeEngine()

			jobID := uuid.New()
			taskID := uuid.New()
			atomID := uuid.New()

			taskSvc := &fakeTaskService{tasks: models.Tasks{
				{ID: taskID, JobID: jobID, AtomID: atomID, Retries: tc.retries},
			}}
			persistGraph(t, db, taskSvc.tasks, nil)
			atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{
				atomID: fakeModelAtom(atomID),
			}}

			// The `sleep 120` of the integration scenario: long enough that the
			// cancel provably lands mid-flight.
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

			// Wait for the container to exist before cancelling: cancelling
			// before the atom is created would prove nothing about reaching it.
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

			require.Len(t, engine.createRequestsForTask(taskID), 1,
				"a cancelled run must not spend its retry budget: the cancel ends the task, it does not start the next attempt")

			var row models.TaskRun
			require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runRecord.ID, taskID).First(&row).Error)
			require.Equal(t, string(run.TaskStatusCancelled), row.Status,
				"the cancelled row must stay cancelled — no later write may resurrect it")
		})
	}
}

// TestRetryTaskRefusesTerminalRow pins the store half directly: the in-run
// retry is the one write that could resurrect a terminal row, and it must
// refuse with the same sentinel its fanned twin uses.
func TestRetryTaskRefusesTerminalRow(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)

	jobID := uuid.New()
	taskID := uuid.New()
	atomID := uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "retry-guard"}).Error)
	require.NoError(t, db.Create(fakeModelAtom(atomID)).Error)
	task := &models.Task{ID: taskID, JobID: jobID, AtomID: atomID, Name: "a"}
	require.NoError(t, db.Create(task).Error)

	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)
	require.NoError(t, store.RegisterTasks(runRecord.ID, []run.RegisterTaskInput{
		{Task: task, Atom: fakeModelAtom(atomID), OutstandingPredecessors: 0},
	}))

	require.NoError(t, store.CancelRun(context.Background(), runRecord.ID))

	require.ErrorIs(t, store.RetryTask(runRecord.ID, taskID, 2), run.ErrTaskInstanceNotRetryable,
		"retrying a cancelled row must be refused, not silently flip it back to pending")

	var row models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runRecord.ID, taskID).First(&row).Error)
	require.Equal(t, string(run.TaskStatusCancelled), row.Status)
}

// TestRunLocalWaitErrorStopsAtom pins the OTHER door of the executor's select,
// the one an arm64 unit failure exposed.
//
// When a run is cancelled, engine.Wait returns ctx.Err() at the same instant
// taskCtx.Done() closes, so both select cases are ready and Go picks between
// them uniformly at random — roughly half of all cancels arrive through
// waitResult, which used to return the error without stopping anything and
// left exactly the orphaned container the cancel path exists to kill. The door
// cannot be selected on purpose, so this drives the code behind it directly: a
// failing Wait with a LIVE task context must still force-stop the atom, which
// is also what the distributed worker's monitorTask has always done.
func TestRunLocalWaitErrorStopsAtom(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
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

	engine.runDurationByName[taskID.String()] = 10 * time.Second
	engine.waitErrByName[taskID.String()] = errors.New("engine wait failed")

	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  1,
		TaskFailurePolicy: taskFailurePolicyHalt,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, &fakeTaskEdgeService{}, engine)

	err := New(&models.Job{ID: jobID}, opts...).Run(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "engine wait failed",
		"the wait failure is the cause worth surfacing")

	require.True(t, engine.wasForceStopped(taskID.String()),
		"a failed Wait means we stopped watching, not that the container stopped — it must be force-stopped, not abandoned")
}
