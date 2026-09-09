package dispatch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// rejectingServer is a stub worker that refuses every dispatch with `code`
// while `full` is set, and accepts once it is cleared.  It answers with the
// real 409 body the handler sends, so the loop under test is reading the same
// wire format production does.  code defaults to no_capacity.
type rejectingServer struct {
	code     string
	full     atomic.Bool
	attempts atomic.Int32
	accepted atomic.Int32
}

func (s *rejectingServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.attempts.Add(1)
		if s.full.Load() {
			code := s.code
			if code == "" {
				code = ReasonNoCapacity
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(ErrorResponse{
				Code:    code,
				Message: "worker busy; task returned to dispatch pool",
			})
			return
		}
		s.accepted.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}
}

// TestCapacityBackoffSchedule pins the schedule itself: each consecutive
// capacity rejection doubles the cooldown up to the cap, the task is held back
// only while the cooldown is live, and an acceptance wipes the slate.
func TestCapacityBackoffSchedule(t *testing.T) {
	l := NewDispatchLoop(DispatchLoopConfig{ProgressDeadline: time.Hour})
	runID, taskID := uuid.New(), uuid.New()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	want := []time.Duration{
		250 * time.Millisecond,
		500 * time.Millisecond,
		time.Second,
		2 * time.Second,
		4 * time.Second,
		5 * time.Second, // capped
		5 * time.Second, // stays capped
	}
	now := base
	for i, expect := range want {
		delay, stalled := l.noteCapacityRejection(runID, taskID, now)
		require.Equal(t, expect, delay, "rejection %d", i+1)
		require.False(t, stalled, "the progress deadline is an hour away")
		require.True(t, l.capacityDelayed(runID, taskID, now.Add(delay-time.Millisecond)),
			"task must be held back while the cooldown is live")
		require.False(t, l.capacityDelayed(runID, taskID, now.Add(delay)),
			"task must be dispatchable again once the cooldown lapses")
		now = now.Add(delay)
	}

	// A lapsed cooldown must not reset the schedule — otherwise every retry
	// starts at the base delay and the backoff is a fixed one-tick retry.
	delay, _ := l.noteCapacityRejection(runID, taskID, now)
	require.Equal(t, 5*time.Second, delay, "a lapsed cooldown keeps the accumulated delay")

	// Acceptance does reset it: the task made progress.
	l.clearCapacityBackoff(runID, taskID)
	require.False(t, l.capacityDelayed(runID, taskID, now), "acceptance clears the cooldown")
	delay, _ = l.noteCapacityRejection(runID, taskID, now)
	require.Equal(t, 250*time.Millisecond, delay, "the next streak starts at the base delay")

	// Sibling tasks are independent: one parked instance never parks the group.
	other := uuid.New()
	require.False(t, l.capacityDelayed(runID, other, now))
}

// TestCapacityBackoffProgressDeadline pins the stall signal: a task that has
// never been accepted surfaces once the progress deadline passes, then stays
// quiet until the next window rather than re-firing on every attempt.
func TestCapacityBackoffProgressDeadline(t *testing.T) {
	l := NewDispatchLoop(DispatchLoopConfig{ProgressDeadline: time.Minute})
	runID, taskID := uuid.New(), uuid.New()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	_, stalled := l.noteCapacityRejection(runID, taskID, base)
	require.False(t, stalled, "the first rejection is ordinary backpressure")

	_, stalled = l.noteCapacityRejection(runID, taskID, base.Add(59*time.Second))
	require.False(t, stalled, "still inside the deadline")

	_, stalled = l.noteCapacityRejection(runID, taskID, base.Add(time.Minute))
	require.True(t, stalled, "a task unaccepted for the whole deadline must surface")

	_, stalled = l.noteCapacityRejection(runID, taskID, base.Add(90*time.Second))
	require.False(t, stalled, "one wedged task must not re-fire every attempt")

	_, stalled = l.noteCapacityRejection(runID, taskID, base.Add(2*time.Minute))
	require.True(t, stalled, "the signal re-arms once per deadline window")

	// Progress restarts the clock: an accepted task that is later refused again
	// is not instantly stale.
	l.clearCapacityBackoff(runID, taskID)
	_, stalled = l.noteCapacityRejection(runID, taskID, base.Add(3*time.Minute))
	require.False(t, stalled, "acceptance restarts the progress-deadline clock")
}

// TestDispatchLoop_CapacityRejectionBoundsPostAttempts is the regression test
// for #400: with a saturated worker the loop used to re-post the same ready task
// on every single tick (measured at ~108 rejections in 124s for one task).  The
// tick count here is deliberately far larger than the attempt bound — the whole
// point is that attempts track the backoff schedule, not the tick rate.
func TestDispatchLoop_CapacityRejectionBoundsPostAttempts(t *testing.T) {
	metrics.Register()

	worker := &rejectingServer{}
	worker.full.Store(true)
	server := httptest.NewServer(worker.handler())
	t.Cleanup(server.Close)

	nodeID, apiPort := serverNodeID(t, server)

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)

	runID := uuid.New()
	_, err := ls.AcquireLease(context.Background(), runID, nodeID, 30*time.Second)
	require.NoError(t, err)
	taskID := uuid.New()
	insertPendingTask(t, store, runID, taskID)

	loop := NewDispatchLoop(DispatchLoopConfig{
		NodeID:     nodeID,
		APIPort:    apiPort,
		Token:      loopToken,
		Interval:   time.Millisecond,
		BatchSize:  64,
		Deadline:   5 * time.Minute,
		LeaseStore: &testOwnerReader{ls},
		Store:      &testTaskReader{store},
		Peers:      &testPeerLister{},
	})
	// Compress the schedule so the test spends milliseconds, not seconds.
	loop.capacityBackoffBase = 40 * time.Millisecond
	loop.capacityBackoffMax = 160 * time.Millisecond

	ctx := context.Background()
	ticks := 0
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		loop.tick(ctx)
		ticks++
	}

	require.Greater(t, ticks, 20, "the loop must actually have ticked many times")
	attempts := int(worker.attempts.Load())
	require.GreaterOrEqual(t, attempts, 1, "the task must be tried at least once")
	// Schedule over 400ms: 40 + 80 + 160 + 160 → at most ~5 attempts, plus slack
	// for a slow host.  Without backoff this equals `ticks`.
	require.LessOrEqual(t, attempts, 8,
		"a saturated worker must not be re-posted once per tick (ticks=%d attempts=%d)", ticks, attempts)
	require.Less(t, attempts, ticks/2,
		"attempts must track the backoff schedule, not the tick rate (ticks=%d attempts=%d)", ticks, attempts)

	// Capacity returns: the task must start on the first tick after the current
	// cooldown lapses — the backoff may cost at most one step, never more.
	worker.full.Store(false)
	time.Sleep(loop.capacityBackoffMax + 20*time.Millisecond)
	loop.tick(ctx)
	require.Equal(t, int32(1), worker.accepted.Load(),
		"one tick after the cooldown lapses must dispatch the task")
	require.False(t, loop.capacityDelayed(runID, taskID, time.Now()),
		"acceptance must clear the task's cooldown")
}

// TestDispatchLoop_CapacityStallSurfacesMetric drives the progress deadline
// through the loop itself: a worker that never has capacity must eventually
// increment caesium_dispatch_stalled_total rather than failing silently.
func TestDispatchLoop_CapacityStallSurfacesMetric(t *testing.T) {
	metrics.Register()

	worker := &rejectingServer{}
	worker.full.Store(true)
	server := httptest.NewServer(worker.handler())
	t.Cleanup(server.Close)

	nodeID, apiPort := serverNodeID(t, server)

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)

	runID := uuid.New()
	_, err := ls.AcquireLease(context.Background(), runID, nodeID, 30*time.Second)
	require.NoError(t, err)
	insertPendingTask(t, store, runID, uuid.New())

	before := counterVecValue(t, metrics.DispatchStalledTotal, ReasonNoCapacity)

	loop := NewDispatchLoop(DispatchLoopConfig{
		NodeID:           nodeID,
		APIPort:          apiPort,
		Token:            loopToken,
		Interval:         time.Millisecond,
		BatchSize:        64,
		Deadline:         5 * time.Minute,
		ProgressDeadline: 20 * time.Millisecond,
		LeaseStore:       &testOwnerReader{ls},
		Store:            &testTaskReader{store},
		Peers:            &testPeerLister{},
	})
	loop.capacityBackoffBase = 5 * time.Millisecond
	loop.capacityBackoffMax = 10 * time.Millisecond

	ctx := context.Background()
	stalled := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		loop.tick(ctx)
		if counterVecValue(t, metrics.DispatchStalledTotal, ReasonNoCapacity) > before {
			stalled = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	require.True(t, stalled,
		"a task refused for the whole progress deadline must increment caesium_dispatch_stalled_total")
}

// TestPostOne_SkipsTaskTerminalSinceSnapshot covers the other half of the #400
// report — "the loop keeps dispatching a producer that already completed".  The
// ready set is snapshotted once per tick and each task is posted from a bounded
// goroutine pool, so a task can go terminal (a cancel, a fail-fast skip, a
// completion) while its own post is still queued.  Posting it then is at best a
// guaranteed rejection and at worst an execution of resolved work.
func TestPostOne_SkipsTaskTerminalSinceSnapshot(t *testing.T) {
	metrics.Register()

	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	nodeID, apiPort := serverNodeID(t, server)

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)

	runID, catalogTaskID, instanceIDs := seedFannedRun(t, db, store)
	_, err := ls.AcquireLease(context.Background(), runID, nodeID, 30*time.Second)
	require.NoError(t, err)

	mgr := run.NewOwnerManager(store, run.CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	_, err = mgr.Recover(runID, 1)
	require.NoError(t, err)

	loop := NewDispatchLoop(DispatchLoopConfig{
		NodeID:       nodeID,
		APIPort:      apiPort,
		Token:        loopToken,
		Interval:     50 * time.Millisecond,
		BatchSize:    64,
		Deadline:     5 * time.Minute,
		LeaseTTL:     30 * time.Second,
		LeaseStore:   &testOwnerReader{ls},
		Store:        &testTaskReader{store},
		Peers:        &testPeerLister{},
		OwnerManager: mgr,
	})

	target := peer{nodeID: nodeID, baseURL: server.URL}
	newReq := func(instance uuid.UUID) DispatchRequest {
		return DispatchRequest{
			RunID:           runID,
			TaskID:          catalogTaskID,
			TaskRunID:       instance,
			OwnerGeneration: 1,
			Attempt:         1,
			WorkerNode:      nodeID,
			OwnerBaseURL:    server.URL,
			Deadline:        time.Now().UTC().Add(time.Minute),
		}
	}

	// Resolve the first instance the way a real completion does: the row is
	// claimed and running, then the worker reports it.
	done := instanceIDs[0]
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", done).
		Updates(map[string]any{"status": string(run.TaskStatusRunning), "claimed_by": nodeID}).Error)
	mgr.MarkDispatched(runID, done, nodeID, 1, time.Now().Add(time.Minute).UnixMilli())
	res, err := mgr.CompleteInstance(runID, catalogTaskID, done, run.TaskStatusSucceeded,
		"success", "", nodeID, nil, nil, nil)
	require.NoError(t, err)
	require.True(t, res.Owned)
	require.False(t, mgr.Dispatchable(runID, done), "a completed instance is no longer dispatchable")

	loop.postOne(context.Background(), runID, target, newReq(done), false)
	require.Equal(t, int32(0), received.Load(),
		"a task that went terminal since the ready snapshot must never reach the wire")

	// Positive control: its still-pending sibling is dispatched normally, so the
	// guard is not simply refusing everything.
	pending := instanceIDs[1]
	require.True(t, mgr.Dispatchable(runID, pending))
	loop.postOne(context.Background(), runID, target, newReq(pending), false)
	require.Equal(t, int32(1), received.Load(), "a live task must still be dispatched")
}

// TestDispatchLoop_RejectionReasonIsMetricLabel pins the two things the
// owner-memory lane's assertion depends on, and the scope of this fix.
//
// A worker's rejection code becomes the caesium_dispatch_rejected_total reason,
// so "how much is this workload being refused for CAPACITY" is answerable at
// all: before, saturation and a stale claim both landed in one worker_rejected
// bucket, and a foreign run spinning on task_not_running was indistinguishable
// from this one being refused for a slot.
//
// And only a capacity rejection is backed off.  task_not_running says the
// dispatch itself was wrong, not "ask later" — the owner's next tick may resolve
// it against a different peer or a moved-on state, so it is deliberately still
// retried immediately.  (A task_not_running spin has been observed in CI, but
// damping it is a different fix from this one: the divergence that produces it
// is the bug.)
func TestDispatchLoop_RejectionReasonIsMetricLabel(t *testing.T) {
	metrics.Register()

	worker := &rejectingServer{code: ReasonTaskNotRunning}
	worker.full.Store(true)
	server := httptest.NewServer(worker.handler())
	t.Cleanup(server.Close)

	nodeID, apiPort := serverNodeID(t, server)

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)

	runID := uuid.New()
	_, err := ls.AcquireLease(context.Background(), runID, nodeID, 30*time.Second)
	require.NoError(t, err)
	taskID := uuid.New()
	insertPendingTask(t, store, runID, taskID)

	beforeStale := counterVecValue(t, metrics.DispatchRejectedTotal, ReasonTaskNotRunning)
	beforeCapacity := counterVecValue(t, metrics.DispatchRejectedTotal, ReasonNoCapacity)

	loop := NewDispatchLoop(DispatchLoopConfig{
		NodeID:     nodeID,
		APIPort:    apiPort,
		Token:      loopToken,
		Interval:   time.Millisecond,
		BatchSize:  64,
		Deadline:   5 * time.Minute,
		LeaseStore: &testOwnerReader{ls},
		Store:      &testTaskReader{store},
		Peers:      &testPeerLister{},
	})
	loop.capacityBackoffBase = time.Hour // would park the task for the whole test

	ctx := context.Background()
	for range 3 {
		loop.tick(ctx)
	}

	require.Equal(t, int32(3), worker.attempts.Load(),
		"a non-capacity rejection is not backed off: every tick must still post")
	require.False(t, loop.capacityDelayed(runID, taskID, time.Now()),
		"only a capacity rejection may park a task")

	require.Equal(t, beforeStale+3, counterVecValue(t, metrics.DispatchRejectedTotal, ReasonTaskNotRunning),
		"the worker's rejection code must be the metric's reason label")
	require.Equal(t, beforeCapacity, counterVecValue(t, metrics.DispatchRejectedTotal, ReasonNoCapacity),
		"a stale-claim rejection must not land in the capacity bucket the lane measures")
}
