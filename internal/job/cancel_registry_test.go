package job

import (
	"context"
	"errors"
	"sync"
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

// TestCancelReconcilerCancelsARunWhoseCancelEventWasDropped is the whole point
// of the sweep.
//
// The in-process bus does not queue: Publish's send is a non-blocking select and
// a full subscriber buffer silently loses the event. run_cancelled is published
// exactly once per cancel, and in local mode the cancel-registry subscriber is
// the ONLY thing that turns it into a stopped container — so losing that one
// message used to orphan the container permanently.
//
// The drop is simulated the only way that is deterministic: the store here has
// no bus at all, so CancelRun writes the row exactly as it does in production
// and the registry never hears about it. That is byte-identical, from the
// registry's side, to the buffer-full drop. Everything downstream of the drop is
// real — the real *run.Store, the real row write, the real batched read.
func TestCancelReconcilerCancelsARunWhoseCancelEventWasDropped(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "cancel-reconcile"}).Error)
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	registry := newCancelRegistry()
	runCtx, release := registry.register(context.Background(), runRecord.ID)
	defer release()

	// The cancel that the registry never hears about.
	require.NoError(t, store.CancelRun(context.Background(), runRecord.ID))
	require.NoError(t, runCtx.Err(), "no event was delivered, so nothing has cancelled the context yet")

	reconciler := newCancelReconciler(registry, store)
	require.Equal(t, 1, reconciler.reconcile(context.Background()),
		"the sweep must cancel the run whose event was lost")
	require.ErrorIs(t, runCtx.Err(), context.Canceled)
}

// The sweep is a background loop over every run this node is executing, so
// "does nothing when there is nothing to do" is a correctness property, not a
// nicety: a sweep that cancelled a healthy run would kill jobs on a timer.
func TestCancelReconcilerLeavesRunningRunsAlone(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "cancel-reconcile-noop"}).Error)
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	registry := newCancelRegistry()
	runCtx, release := registry.register(context.Background(), runRecord.ID)
	defer release()

	reconciler := newCancelReconciler(registry, store)
	for range 3 {
		require.Zero(t, reconciler.reconcile(context.Background()))
	}
	require.NoError(t, runCtx.Err(), "a running run must survive every sweep")
}

// The sweep must also leave alone the terminal statuses that are NOT a
// cancellation. `succeeded`/`failed` are written by the engine's own completion
// defer, which is still using the run context to finalize the run at that
// instant; cancelling there would abort a correct completion. `skipped` is a
// run created terminal by the dataset-hold gate and never has an engine.
func TestCancelReconcilerIgnoresNonCancelledTerminalRuns(t *testing.T) {
	for _, status := range []run.Status{run.StatusSucceeded, run.StatusFailed, run.StatusSkipped} {
		t.Run(string(status), func(t *testing.T) {
			db := jobdeftestutil.OpenTestDB(t)
			t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

			store := run.NewStore(db)
			jobID := uuid.New()
			require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "cancel-reconcile-terminal"}).Error)
			runRecord, err := store.Start(jobID, nil)
			require.NoError(t, err)
			require.NoError(t, db.Model(&models.JobRun{}).
				Where("id = ?", runRecord.ID).
				Update("status", string(status)).Error)

			registry := newCancelRegistry()
			runCtx, release := registry.register(context.Background(), runRecord.ID)
			defer release()

			require.Zero(t, newCancelReconciler(registry, store).reconcile(context.Background()))
			require.NoError(t, runCtx.Err())
		})
	}
}

// The sweep must count and narrate a lost cancel exactly ONCE, and it must not
// count a cancel that the event already delivered.
//
// Both hazards come from the same fact: cancel() leaves entries in the registry
// for their own release funcs to remove, so a cancelled run — however it was
// cancelled — stays in the sweep's input for as long as its engine takes to
// drain. A sweep that counted every registered entry of every cancelled run
// would increment caesium_run_cancel_reconciled_total on every ordinary
// cancellation and log the same incident once per tick, which would make the
// one metric that means "the bus lost an event" mean nothing at all.
func TestCancelReconcilerCountsOnlyContextsNothingElseHadCancelled(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "cancel-reconcile-once"}).Error)

	dropped, err := store.Start(jobID, nil)
	require.NoError(t, err)
	delivered, err := store.Start(jobID, nil)
	require.NoError(t, err)

	registry := newCancelRegistry()
	_, releaseDropped := registry.register(context.Background(), dropped.ID)
	defer releaseDropped()
	deliveredCtx, releaseDelivered := registry.register(context.Background(), delivered.ID)
	defer releaseDelivered()

	require.NoError(t, store.CancelRun(context.Background(), dropped.ID))
	require.NoError(t, store.CancelRun(context.Background(), delivered.ID))
	// The event reached this one: its context is already cancelled, but its
	// entry is still registered because the engine has not finished draining.
	require.Equal(t, 1, registry.cancel(delivered.ID))
	require.ErrorIs(t, deliveredCtx.Err(), context.Canceled)

	reconciler := newCancelReconciler(registry, store)
	require.Equal(t, 1, reconciler.reconcile(context.Background()),
		"only the run whose event was lost is the sweep's work; the other was already stopped")
	require.Zero(t, reconciler.reconcile(context.Background()),
		"a run the sweep has already cancelled is not a fresh incident on the next tick")
}

// countingLookup stands in for the run store so the loop's lifecycle can be
// driven on a millisecond ticker without a database.
type countingLookup struct {
	mu    sync.Mutex
	calls int
}

func (c *countingLookup) CancelledRunIDs(context.Context, []uuid.UUID) ([]uuid.UUID, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return nil, nil
}

func (c *countingLookup) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// The sweep runs for the life of the server, so it has to die with it: a loop
// that outlived the shutdown context would be a goroutine leak in every test
// binary and every restart. (`just unit-test` runs with -race, which is what
// makes the concurrent reads here meaningful.)
func TestStartRunCancelReconcilerStopsWithItsContext(t *testing.T) {
	// The loop sweeps defaultCancelRegistry, so give it something to find.
	runID := uuid.New()
	_, release := RegisterRunCancel(context.Background(), runID)
	defer release()

	lookup := &countingLookup{}
	ctx, cancel := context.WithCancel(context.Background())
	StartRunCancelReconciler(ctx, lookup, time.Millisecond)

	require.Eventually(t, func() bool { return lookup.count() > 0 }, 5*time.Second, time.Millisecond,
		"the reconciler never swept")

	cancel()
	require.Eventually(t, func() bool {
		before := lookup.count()
		time.Sleep(50 * time.Millisecond)
		return lookup.count() == before
	}, 5*time.Second, 10*time.Millisecond, "the reconciler kept sweeping after its context was cancelled")
}

func TestStartRunCancelReconcilerIsDisabledByANonPositiveInterval(t *testing.T) {
	lookup := &countingLookup{}
	StartRunCancelReconciler(t.Context(), lookup, 0)
	StartRunCancelReconciler(t.Context(), nil, time.Millisecond)

	time.Sleep(50 * time.Millisecond)
	require.Zero(t, lookup.count(), "interval 0 is the documented off switch; it must start no loop at all")
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
