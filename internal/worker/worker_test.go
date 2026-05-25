package worker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/dispatch"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

func TestWorkerRunExecutesClaimedTasks(t *testing.T) {
	var executed int32
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	claimer := &sequenceClaimer{
		responses: []claimerResponse{
			{task: &models.TaskRun{ID: uuid.New()}},
			{task: &models.TaskRun{ID: uuid.New()}},
		},
	}

	worker := NewWorker(claimer, NewPool(2), time.Millisecond, func(_ context.Context, _ *models.TaskRun) {
		if atomic.AddInt32(&executed, 1) == 2 {
			cancel()
		}
	})

	if err := worker.Run(ctx); err != nil {
		t.Fatalf("worker run failed: %v", err)
	}

	if got := atomic.LoadInt32(&executed); got != 2 {
		t.Fatalf("expected 2 executed tasks, got %d", got)
	}
}

func TestWorkerRunContinuesAfterClaimErrors(t *testing.T) {
	var executed int32
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	claimer := &sequenceClaimer{
		responses: []claimerResponse{
			{err: errors.New("transient claim failure")},
			{task: &models.TaskRun{ID: uuid.New()}},
		},
	}

	worker := NewWorker(claimer, NewPool(1), time.Millisecond, func(_ context.Context, _ *models.TaskRun) {
		atomic.AddInt32(&executed, 1)
		cancel()
	})

	if err := worker.Run(ctx); err != nil {
		t.Fatalf("worker run failed: %v", err)
	}

	if got := atomic.LoadInt32(&executed); got != 1 {
		t.Fatalf("expected 1 executed task, got %d", got)
	}
}

func TestWorkerRunReclaimsWhenDueBeforeBusyClaimLoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	claimer := &reclaimingSequenceClaimer{
		sequenceClaimer: sequenceClaimer{
			responses: []claimerResponse{
				{task: &models.TaskRun{ID: uuid.New()}},
				{task: &models.TaskRun{ID: uuid.New()}},
			},
		},
	}

	worker := NewWorker(claimer, NewPool(1), time.Millisecond, func(_ context.Context, _ *models.TaskRun) {
		cancel()
	}).WithReclaimInterval(time.Hour)
	worker.lastReclaim = time.Now().Add(-2 * time.Hour)

	if err := worker.Run(ctx); err != nil {
		t.Fatalf("worker run failed: %v", err)
	}

	if got := atomic.LoadInt32(&claimer.reclaims); got != 1 {
		t.Fatalf("expected 1 reclaim attempt, got %d", got)
	}
}

func TestWorkerRunSkipsReclaimWhenGateDenies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	claimer := &reclaimingSequenceClaimer{
		sequenceClaimer: sequenceClaimer{
			responses: []claimerResponse{{task: &models.TaskRun{ID: uuid.New()}}},
		},
	}

	worker := NewWorker(claimer, NewPool(1), time.Millisecond, func(_ context.Context, _ *models.TaskRun) {
		cancel()
	}).WithReclaimInterval(time.Hour).
		WithReclaimGate(ReclaimGateFunc(func(context.Context) (bool, error) {
			return false, nil
		}))
	worker.lastReclaim = time.Now().Add(-2 * time.Hour)

	if err := worker.Run(ctx); err != nil {
		t.Fatalf("worker run failed: %v", err)
	}

	if got := atomic.LoadInt32(&claimer.reclaims); got != 0 {
		t.Fatalf("expected no reclaim attempt, got %d", got)
	}
}

func TestSleepWithContextHandlesTinyDurations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := sleepWithContext(ctx, time.Nanosecond); err != nil {
		t.Fatalf("sleepWithContext returned error: %v", err)
	}
}

type claimerResponse struct {
	task *models.TaskRun
	err  error
}

type sequenceClaimer struct {
	mu        sync.Mutex
	responses []claimerResponse
}

func (s *sequenceClaimer) ClaimNext(context.Context) (*models.TaskRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.responses) == 0 {
		return nil, nil
	}

	resp := s.responses[0]
	s.responses = s.responses[1:]
	return resp.task, resp.err
}

type reclaimingSequenceClaimer struct {
	sequenceClaimer
	reclaims int32
}

func (s *reclaimingSequenceClaimer) ReclaimExpired(context.Context) error {
	atomic.AddInt32(&s.reclaims, 1)
	return nil
}

// fakeLeaseRenewer records calls to RenewLeases for test assertions. By
// default each call returns rowsAffected == len(ids); a test can override via
// rowsAffectedFn to simulate a partial match (e.g. reassigned claim).
type fakeLeaseRenewer struct {
	mu             sync.Mutex
	calls          []renewCall
	rowsAffectedFn func(nodeID string, ids []uuid.UUID) int64
}

type renewCall struct {
	nodeID    string
	ids       []uuid.UUID
	expiresAt time.Time
}

func (f *fakeLeaseRenewer) RenewLeases(_ context.Context, nodeID string, ids []uuid.UUID, expiresAt time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]uuid.UUID, len(ids))
	copy(cp, ids)
	f.calls = append(f.calls, renewCall{nodeID: nodeID, ids: cp, expiresAt: expiresAt})
	if f.rowsAffectedFn != nil {
		return f.rowsAffectedFn(nodeID, cp), nil
	}
	return int64(len(cp)), nil
}

func (f *fakeLeaseRenewer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeLeaseRenewer) lastCall() (renewCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return renewCall{}, false
	}
	return f.calls[len(f.calls)-1], true
}

// makeTask constructs a TaskRun with the given nodeID and claim expiry.
func makeTask(nodeID string, claimExpiresAt time.Time) *models.TaskRun {
	tr := &models.TaskRun{
		ID:        uuid.New(),
		TaskID:    uuid.New(),
		JobRunID:  uuid.New(),
		ClaimedBy: nodeID,
	}
	tr.ClaimExpiresAt = &claimExpiresAt
	return tr
}

// TestBatchedRenewal_NoInflightNoUpdate verifies that with zero in-flight
// claims, renewLeasesNow never calls the LeaseRenewer.
func TestBatchedRenewal_NoInflightNoUpdate(t *testing.T) {
	renewer := &fakeLeaseRenewer{}
	leaseTTL := 5 * time.Minute
	w := NewWorker(&sequenceClaimer{}, NewPool(1), time.Millisecond, nil).
		WithLeaseRenewal(renewer, leaseTTL, 0)

	w.renewLeasesNow(t.Context())

	if got := renewer.callCount(); got != 0 {
		t.Fatalf("expected 0 RenewLeases calls with no in-flight tasks, got %d", got)
	}
}

// TestBatchedRenewal_NInflightOneUpdate verifies that with N in-flight claims
// all within lease_ttl/2 of expiry, exactly one RenewLeases call is issued
// covering all of them.
func TestBatchedRenewal_NInflightOneUpdate(t *testing.T) {
	renewer := &fakeLeaseRenewer{}
	leaseTTL := 5 * time.Minute
	w := NewWorker(&sequenceClaimer{}, NewPool(1), time.Millisecond, nil).
		WithLeaseRenewal(renewer, leaseTTL, 0)

	nodeID := "node-a"
	// Add 4 in-flight tasks that expire in 1 minute (< halfTTL = 2.5 min).
	imminent := time.Now().Add(time.Minute)
	ids := make(map[uuid.UUID]struct{}, 4)
	for i := 0; i < 4; i++ {
		task := makeTask(nodeID, imminent)
		ids[task.ID] = struct{}{}
		w.trackInFlight(task)
	}

	w.renewLeasesNow(t.Context())

	if got := renewer.callCount(); got != 1 {
		t.Fatalf("expected exactly 1 RenewLeases call, got %d", got)
	}
	call, _ := renewer.lastCall()
	if call.nodeID != nodeID {
		t.Fatalf("expected nodeID %q, got %q", nodeID, call.nodeID)
	}
	if len(call.ids) != len(ids) {
		t.Fatalf("expected %d IDs in batched UPDATE, got %d", len(ids), len(call.ids))
	}
	for _, id := range call.ids {
		if _, ok := ids[id]; !ok {
			t.Fatalf("unexpected ID %s in batched UPDATE", id)
		}
	}
}

// TestBatchedRenewal_SkipWhenNotNeeded verifies that no UPDATE is issued when
// all in-flight claims have claim_expires_at > now + lease_ttl/2.
func TestBatchedRenewal_SkipWhenNotNeeded(t *testing.T) {
	renewer := &fakeLeaseRenewer{}
	leaseTTL := 5 * time.Minute
	w := NewWorker(&sequenceClaimer{}, NewPool(1), time.Millisecond, nil).
		WithLeaseRenewal(renewer, leaseTTL, 0)

	nodeID := "node-a"
	// Tasks expire in 4 minutes — well beyond halfTTL of 2.5 minutes.
	distant := time.Now().Add(4 * time.Minute)
	for i := 0; i < 3; i++ {
		w.trackInFlight(makeTask(nodeID, distant))
	}

	w.renewLeasesNow(t.Context())

	if got := renewer.callCount(); got != 0 {
		t.Fatalf("expected 0 RenewLeases calls (skip-when-not-needed), got %d", got)
	}
}

// TestBatchedRenewal_OtherNodeNotTouched registers in-flight claims for two
// different nodes (shouldn't happen in production, but the safety must hold).
// renewLeasesNow must issue one UPDATE per node, each containing only that
// node's IDs — never sending a cross-node ID into another node's WHERE clause.
func TestBatchedRenewal_OtherNodeNotTouched(t *testing.T) {
	renewer := &fakeLeaseRenewer{}
	leaseTTL := 5 * time.Minute
	w := NewWorker(&sequenceClaimer{}, NewPool(1), time.Millisecond, nil).
		WithLeaseRenewal(renewer, leaseTTL, 0)

	nodeA := "node-a"
	nodeB := "node-b"
	imminent := time.Now().Add(time.Minute) // within halfTTL -> renewal needed

	taskA := makeTask(nodeA, imminent)
	taskB := makeTask(nodeB, imminent)

	w.trackInFlight(taskA)
	// Directly insert a node-b entry into the in-flight map to simulate a
	// cross-node scenario.
	w.inFlightMu.Lock()
	w.inFlight[taskB.ID] = &inFlightClaim{claimedBy: nodeB, claimExpiresAt: imminent}
	w.inFlightMu.Unlock()

	w.renewLeasesNow(t.Context())

	// Exactly one UPDATE per node, never mixing IDs across nodes.
	if got := renewer.callCount(); got != 2 {
		t.Fatalf("expected 2 RenewLeases calls (one per node), got %d", got)
	}
	seen := map[string]uuid.UUID{}
	renewer.mu.Lock()
	for _, call := range renewer.calls {
		if len(call.ids) != 1 {
			t.Fatalf("expected each call to carry exactly its node's ID, got %d for %q", len(call.ids), call.nodeID)
		}
		seen[call.nodeID] = call.ids[0]
	}
	renewer.mu.Unlock()
	if got, want := seen[nodeA], taskA.ID; got != want {
		t.Fatalf("node-a UPDATE got id %s, want %s", got, want)
	}
	if got, want := seen[nodeB], taskB.ID; got != want {
		t.Fatalf("node-b UPDATE got id %s, want %s", got, want)
	}
}

// TestBatchedRenewal_ZeroLeaseTTLNoRenewal verifies that constructing a worker
// with leaseTTL == 0 disables the renewal goroutine entirely; a zero TTL would
// otherwise make every tick set claim_expires_at = now, immediately expiring
// every in-flight lease and causing all running tasks to be reclaimed.
func TestBatchedRenewal_ZeroLeaseTTLNoRenewal(t *testing.T) {
	renewer := &fakeLeaseRenewer{}
	w := NewWorker(&sequenceClaimer{}, NewPool(1), time.Millisecond, nil).
		WithLeaseRenewal(renewer, 0, 0)

	w.trackInFlight(makeTask("node-a", time.Now().Add(-time.Hour))) // already expired
	w.renewLeasesNow(t.Context())

	if got := renewer.callCount(); got != 0 {
		t.Fatalf("expected 0 RenewLeases calls with zero leaseTTL, got %d", got)
	}
}

// TestBatchedRenewal_ZeroRowsAffectedNoLocalUpdate verifies that when the DB
// reports zero rows affected (every claim was reassigned between snapshot and
// write), the worker neither bumps the counter nor advances the in-memory
// expiry — keeping in-memory state honest with the DB.
func TestBatchedRenewal_ZeroRowsAffectedNoLocalUpdate(t *testing.T) {
	renewer := &fakeLeaseRenewer{
		rowsAffectedFn: func(_ string, _ []uuid.UUID) int64 { return 0 },
	}
	leaseTTL := 5 * time.Minute
	w := NewWorker(&sequenceClaimer{}, NewPool(1), time.Millisecond, nil).
		WithLeaseRenewal(renewer, leaseTTL, 0)

	imminent := time.Now().Add(time.Minute)
	task := makeTask("node-a", imminent)
	w.trackInFlight(task)

	w.renewLeasesNow(t.Context())

	if got := renewer.callCount(); got != 1 {
		t.Fatalf("expected 1 RenewLeases call (still attempted), got %d", got)
	}

	w.inFlightMu.Lock()
	claim := w.inFlight[task.ID]
	w.inFlightMu.Unlock()
	if !claim.claimExpiresAt.Equal(imminent) {
		t.Fatalf("expected in-memory expiry unchanged when rows_affected=0, got %v want %v", claim.claimExpiresAt, imminent)
	}
}

// --- Phase 2 B2: inbound dispatched-task channel ---

// TestSubmitDispatched_NotAccepting verifies that a worker without the inbound
// path enabled rejects dispatched tasks (owner mode off → byte-identical Phase 1).
func TestSubmitDispatched_NotAccepting(t *testing.T) {
	w := NewWorker(&sequenceClaimer{}, NewPool(1), time.Millisecond, nil)
	err := w.SubmitDispatched(dispatch.InboundDispatch{Task: &models.TaskRun{ID: uuid.New()}})
	if !errors.Is(err, ErrWorkerNotAccepting) {
		t.Fatalf("expected ErrWorkerNotAccepting, got %v", err)
	}
}

// TestSubmitDispatched_NilTask guards against a nil task panicking downstream.
func TestSubmitDispatched_NilTask(t *testing.T) {
	w := NewWorker(&sequenceClaimer{}, NewPool(1), time.Millisecond, nil).
		WithInboundDispatch("tok")
	if err := w.SubmitDispatched(dispatch.InboundDispatch{Task: nil}); err == nil {
		t.Fatal("expected error for nil dispatched task")
	}
}

// TestSubmitDispatched_BufferFull verifies that once the inbound buffer is full
// (pool-size-bounded), further submits are rejected with ErrInboundFull so the
// owner can re-dispatch — backpressure is unified with the pool.
func TestSubmitDispatched_BufferFull(t *testing.T) {
	// Pool size 2 → inbound buffer cap 2. Don't run the worker so nothing drains.
	w := NewWorker(&sequenceClaimer{}, NewPool(2), time.Millisecond, nil).
		WithInboundDispatch("tok")

	for i := 0; i < 2; i++ {
		if err := w.SubmitDispatched(dispatch.InboundDispatch{Task: &models.TaskRun{ID: uuid.New()}}); err != nil {
			t.Fatalf("submit %d should succeed (buffer not full yet), got %v", i, err)
		}
	}
	// Third submit overflows the size-2 buffer.
	err := w.SubmitDispatched(dispatch.InboundDispatch{Task: &models.TaskRun{ID: uuid.New()}})
	if !errors.Is(err, ErrInboundFull) {
		t.Fatalf("expected ErrInboundFull on overflow, got %v", err)
	}
}

// TestWorkerRunDrainsInboundDispatch verifies the Run loop drains a dispatched
// task onto the shared pool and executes it with the owner metadata threaded
// through the execution context (so the executor would select the owner sink).
func TestWorkerRunDrainsInboundDispatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var gotMeta dispatchMeta
	var sawMeta bool
	executed := make(chan struct{}, 1)

	// Claimer returns no pull-path work, so the only execution is the dispatched
	// task drained from the inbound channel.
	w := NewWorker(&sequenceClaimer{}, NewPool(2), 50*time.Millisecond,
		func(ec context.Context, task *models.TaskRun) {
			gotMeta, sawMeta = dispatchMetaFrom(ec)
			executed <- struct{}{}
			cancel()
		}).WithInboundDispatch("tok")

	meta := dispatch.InboundDispatch{
		Task:            &models.TaskRun{ID: uuid.New(), TaskID: uuid.New(), JobRunID: uuid.New()},
		OwnerBaseURL:    "http://10.0.0.1:8080",
		OwnerGeneration: 9,
		Attempt:         2,
		WorkerNode:      "10.0.0.5:9001",
	}
	if err := w.SubmitDispatched(meta); err != nil {
		t.Fatalf("SubmitDispatched failed: %v", err)
	}

	if err := w.Run(ctx); err != nil {
		t.Fatalf("worker run failed: %v", err)
	}

	select {
	case <-executed:
	default:
		t.Fatal("dispatched task was never executed")
	}
	if !sawMeta {
		t.Fatal("executor did not receive dispatch metadata via context")
	}
	if gotMeta.OwnerBaseURL != "http://10.0.0.1:8080" || gotMeta.OwnerGeneration != 9 || gotMeta.Attempt != 2 || gotMeta.WorkerNode != "10.0.0.5:9001" {
		t.Fatalf("dispatch metadata mismatch in executor context: %+v", gotMeta)
	}
	if gotMeta.Token != "tok" {
		t.Fatalf("expected completion token 'tok' threaded onto meta, got %q", gotMeta.Token)
	}
}

// TestWorkerRunDrainsInboundAndClaimNext verifies both paths share the pool:
// a dispatched task and a ClaimNext'd task both execute.
func TestWorkerRunDrainsInboundAndClaimNext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var executed int32
	claimer := &sequenceClaimer{
		responses: []claimerResponse{
			{task: &models.TaskRun{ID: uuid.New()}}, // pull-path task
		},
	}
	w := NewWorker(claimer, NewPool(2), 50*time.Millisecond,
		func(_ context.Context, _ *models.TaskRun) {
			if atomic.AddInt32(&executed, 1) == 2 {
				cancel()
			}
		}).WithInboundDispatch("tok")

	// Enqueue one dispatched task; ClaimNext yields one pull-path task.
	if err := w.SubmitDispatched(dispatch.InboundDispatch{Task: &models.TaskRun{ID: uuid.New()}}); err != nil {
		t.Fatalf("SubmitDispatched failed: %v", err)
	}

	if err := w.Run(ctx); err != nil {
		t.Fatalf("worker run failed: %v", err)
	}
	if got := atomic.LoadInt32(&executed); got != 2 {
		t.Fatalf("expected 2 executed tasks (1 dispatched + 1 ClaimNext'd), got %d", got)
	}
}
