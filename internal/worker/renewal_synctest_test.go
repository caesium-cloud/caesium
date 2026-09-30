package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
)

// This file exercises the worker's two lease-renewal goroutines —
// runLeaseRenewal (batched task-claim renewal, worker.go) and
// runRunLeaseRenewal (batched run-lease renewal, Phase 2 owner mode) — inside
// a testing/synctest bubble, per A1's Clock-option table: synctest gives
// isolated Go timer/channel/cancellation logic a deterministic fake clock, but
// real sockets, process lifetime and native dqlite do NOT become deterministic
// under it. Every renewer here is the existing LeaseRenewer/RunLeaseRenewer
// fake (fakeLeaseRenewer, inspectingRenewer, mockRunLeaseRenewer — all defined
// in worker_test.go / run_lease_renewal_test.go, same package) driven through
// a real *Worker's real ticker loop; no production code changed to make this
// possible, and no DB, socket, or goroutine outside the bubble is involved.
//
// What stays out of reach, honestly: whether *run.Store's real
// RenewLeases/RenewOwnedLeases SQL actually behaves this way against dqlite,
// whether a real claim expiry lines up with wall-clock lease_ttl under host
// scheduling jitter, and anything involving the HTTP dispatch path. Those are
// exactly the cases run_lease_renewal_test.go's non-synctest tests (real
// fakes, real goroutines, no fake clock) and B-stream's live cluster already
// own; this file only strengthens the timer/cancellation dimension that used
// to be proven with a real-wall-clock race
// (TestRunLeaseRenewalTickCancelsLostClaim's `time.After(10 * time.Second)`).

// --- Task-claim renewal (runLeaseRenewal) ---

// TestTaskClaimRenewalFiresAtConfiguredIntervalInFakeTime proves the ticker
// cadence itself: with leaseTTL=20s and an override renewInterval=20s/2=10s,
// a claim renewed at tick N always lands exactly at the next tick's due
// threshold (claimExpiresAt - now == halfTTL), so every one of N ticks issues
// exactly one RenewLeases call — deterministically, in fake time, not "wait up
// to some real seconds and hope."
func TestTaskClaimRenewalFiresAtConfiguredIntervalInFakeTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const leaseTTL = 20 * time.Second
		const interval = 10 * time.Second // == leaseTTL/2: keeps the claim perpetually "due"

		renewer := &fakeLeaseRenewer{}
		w := NewWorker(&sequenceClaimer{}, NewPool(1), time.Millisecond, nil).
			WithLeaseRenewal(renewer, leaseTTL, interval)
		trackCancellable(w, "node-a", time.Now())

		ctx, cancel := context.WithCancel(context.Background())
		go w.runLeaseRenewal(ctx)

		const wantTicks = 5
		time.Sleep(wantTicks*interval + interval/2)
		synctest.Wait()

		if got := renewer.callCount(); got != wantTicks {
			t.Fatalf("after %d ticker intervals of fake time, got %d renewal calls, want %d", wantTicks, got, wantTicks)
		}

		cancel()
		synctest.Wait() // the goroutine must exit, or Test() deadlocks below
	})
}

// TestTaskClaimRenewalCancellationStopsWithNoLeak proves cancellation actually
// stops the renewal goroutine — not just that it stops CALLING RenewLeases,
// but that the goroutine itself exits and the ticker is no longer read. If it
// did not exit, synctest.Test would detect the bubble can never go idle again
// once the root function returns and fail; the explicit post-cancel sleep and
// call-count check additionally proves no renewal silently continues.
func TestTaskClaimRenewalCancellationStopsWithNoLeak(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const leaseTTL = 20 * time.Second
		const interval = 10 * time.Second

		renewer := &fakeLeaseRenewer{}
		w := NewWorker(&sequenceClaimer{}, NewPool(1), time.Millisecond, nil).
			WithLeaseRenewal(renewer, leaseTTL, interval)
		trackCancellable(w, "node-a", time.Now())

		ctx, cancel := context.WithCancel(context.Background())
		go w.runLeaseRenewal(ctx)

		time.Sleep(2*interval + interval/2)
		synctest.Wait()
		if got := renewer.callCount(); got != 2 {
			t.Fatalf("expected 2 renewal calls before cancellation, got %d", got)
		}

		cancel()
		synctest.Wait() // waits for the goroutine to observe ctx.Done() and exit

		// Advance fake time well past several more would-be ticks. Nothing
		// fires: the goroutine that read ticker.C is gone.
		time.Sleep(5 * interval)
		if got := renewer.callCount(); got != 2 {
			t.Fatalf("renewal continued after cancellation: got %d calls, want 2", got)
		}
	})
}

// TestTaskClaimRenewalTickSurfacesLeaseLossAsCancellation is the synctest
// replacement for TestRunLeaseRenewalTickCancelsLostClaim
// (run_lease_renewal_test.go), which proves the same claim-loss-cancels-task
// wiring but can only wait on real wall-clock time
// (`time.After(10 * time.Second)`) for the tick to land. Here the tick lands
// at an exact, deterministic point in fake time: a lost claim (the catalog
// holds nothing for this node) must surface as the task's context being
// cancelled — not swallowed as a log line — on the very first tick.
func TestTaskClaimRenewalTickSurfacesLeaseLossAsCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		renewer := &inspectingRenewer{heldIDs: map[uuid.UUID]struct{}{}} // catalog holds nothing
		const leaseTTL = 30 * time.Second
		const interval = 20 * time.Millisecond
		w := NewWorker(&sequenceClaimer{}, NewPool(1), time.Millisecond, nil).
			WithLeaseRenewal(renewer, leaseTTL, interval)

		_, taskCtx := trackCancellable(w, "node-a", time.Now().Add(5*time.Minute))

		ctx, cancel := context.WithCancel(context.Background())
		go w.runLeaseRenewal(ctx)

		time.Sleep(interval)
		synctest.Wait()

		if err := taskCtx.Err(); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected the lost claim to cancel the task's context on the first tick, got %v", err)
		}

		cancel()
		synctest.Wait()
	})
}

// flakyOnceLeaseRenewer fails its Nth call (1-indexed) and succeeds otherwise.
type flakyOnceLeaseRenewer struct {
	mu     sync.Mutex
	calls  int
	failOn int
}

func (f *flakyOnceLeaseRenewer) RenewLeases(_ context.Context, _ string, ids []uuid.UUID, _ time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls == f.failOn {
		return 0, errors.New("transient database blip")
	}
	return int64(len(ids)), nil
}

func (f *flakyOnceLeaseRenewer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestTaskClaimRenewalSurvivesTransientErrorAndRetriesNextTick proves a
// renewal failure is surfaced (attempted, logged, visible in the call count)
// rather than swallowed into a dead loop: after RenewLeases errors on one
// tick, the SAME goroutine must still be alive and try again on the next tick
// — and, since the error is transient/unknown (not a proven claim loss), the
// task must not be cancelled on its account.
func TestTaskClaimRenewalSurvivesTransientErrorAndRetriesNextTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		renewer := &flakyOnceLeaseRenewer{failOn: 1}
		const leaseTTL = 20 * time.Second
		const interval = 10 * time.Second
		w := NewWorker(&sequenceClaimer{}, NewPool(1), time.Millisecond, nil).
			WithLeaseRenewal(renewer, leaseTTL, interval)

		_, taskCtx := trackCancellable(w, "node-a", time.Now())

		ctx, cancel := context.WithCancel(context.Background())
		go w.runLeaseRenewal(ctx)

		time.Sleep(interval + interval/2)
		synctest.Wait()
		if got := renewer.callCount(); got != 1 {
			t.Fatalf("expected 1 attempted call after the first tick, got %d", got)
		}
		if taskCtx.Err() != nil {
			t.Fatalf("a transient renewal error must not cancel the task, got %v", taskCtx.Err())
		}

		time.Sleep(interval)
		synctest.Wait()
		if got := renewer.callCount(); got != 2 {
			t.Fatalf("renewal loop did not retry on the next tick after a transient error: got %d calls, want 2", got)
		}
		if taskCtx.Err() != nil {
			t.Fatalf("the task must still not be cancelled after the retry succeeded, got %v", taskCtx.Err())
		}

		cancel()
		synctest.Wait()
	})
}

// blockingLeaseRenewer blocks its first call on gate, and tracks the number
// currently in flight so a test can assert at most one call ever runs at a
// time — the structural guarantee that makes "a slow renew call does not
// stack overlapping renewals" true regardless of how many ticks the clock
// silently drops while a call is outstanding.
type blockingLeaseRenewer struct {
	mu          sync.Mutex
	calls       int
	inFlight    int
	maxInFlight int
	gate        chan struct{}
	blockFirst  bool
}

func (b *blockingLeaseRenewer) RenewLeases(_ context.Context, _ string, ids []uuid.UUID, _ time.Time) (int64, error) {
	b.mu.Lock()
	b.calls++
	b.inFlight++
	if b.inFlight > b.maxInFlight {
		b.maxInFlight = b.inFlight
	}
	block := b.blockFirst && b.calls == 1
	b.mu.Unlock()

	if block {
		<-b.gate
	}

	b.mu.Lock()
	b.inFlight--
	b.mu.Unlock()
	return int64(len(ids)), nil
}

func (b *blockingLeaseRenewer) snapshot() (calls, maxInFlight int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls, b.maxInFlight
}

// TestTaskClaimRenewalDoesNotStackOverlappingRenewalsWhenSlow proves the
// "a slow renew call does not stack overlapping renewals" requirement two
// ways: (1) while the first call is blocked, however many ticker intervals of
// fake time elapse, no second call is even ATTEMPTED (calls stays at 1) — the
// single select-loop goroutine cannot read ticker.C again until the
// synchronous call it is inside returns; (2) maxInFlight never exceeds 1,
// pinning that structural guarantee against a future refactor that might
// dispatch a tick's renewal onto its own goroutine.
func TestTaskClaimRenewalDoesNotStackOverlappingRenewalsWhenSlow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		renewer := &blockingLeaseRenewer{gate: make(chan struct{}), blockFirst: true}
		const leaseTTL = 20 * time.Second
		const interval = 10 * time.Second
		w := NewWorker(&sequenceClaimer{}, NewPool(1), time.Millisecond, nil).
			WithLeaseRenewal(renewer, leaseTTL, interval)
		trackCancellable(w, "node-a", time.Now())

		ctx, cancel := context.WithCancel(context.Background())
		go w.runLeaseRenewal(ctx)

		// Let several ticker periods elapse while the first call is blocked.
		time.Sleep(5*interval + interval/2)
		synctest.Wait()

		calls, maxInFlight := renewer.snapshot()
		if calls != 1 {
			t.Fatalf("expected exactly 1 attempted call while the first was still blocked (ticks must not queue up as separate calls), got %d", calls)
		}
		if maxInFlight != 1 {
			t.Fatalf("expected at most 1 renewal call in flight at once, observed %d", maxInFlight)
		}

		close(renewer.gate) // release the blocked call
		synctest.Wait()

		calls, maxInFlight = renewer.snapshot()
		if calls < 2 {
			t.Fatalf("expected the loop to resume and consume a coalesced tick after the slow call finished, got %d total calls", calls)
		}
		if maxInFlight != 1 {
			t.Fatalf("expected at most 1 renewal call in flight at once across the whole run, observed %d", maxInFlight)
		}

		cancel()
		synctest.Wait()
	})
}

// --- Run-lease renewal (runRunLeaseRenewal, Phase 2 owner mode) ---

// TestRunLeaseRenewalFiresAtConfiguredIntervalInFakeTime is the run-lease
// counterpart of TestTaskClaimRenewalFiresAtConfiguredIntervalInFakeTime.
// renewRunLeasesNow has no due-gating (unlike the task-claim path): every tick
// unconditionally calls RenewOwnedLeases, so the assertion is simpler — exact
// call count equals exact tick count.
func TestRunLeaseRenewalFiresAtConfiguredIntervalInFakeTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const runLeaseTTL = 20 * time.Second // batchLeaseRenewInterval(ttl, 0) = ttl/4 = 5s
		interval := batchLeaseRenewInterval(runLeaseTTL, 0)

		renewer := &mockRunLeaseRenewer{ownedCount: 2}
		w := &Worker{
			runLeaseRenewer: renewer,
			runLeaseTTL:     runLeaseTTL,
			runLeaseNodeID:  "node-a:9001",
		}

		ctx, cancel := context.WithCancel(context.Background())
		go w.runRunLeaseRenewal(ctx)

		const wantTicks = 4
		time.Sleep(time.Duration(wantTicks)*interval + interval/2)
		synctest.Wait()

		renewer.mu.Lock()
		got := renewer.renewCalls
		renewer.mu.Unlock()
		if got != wantTicks {
			t.Fatalf("after %d ticker intervals of fake time, got %d run-lease renewal calls, want %d", wantTicks, got, wantTicks)
		}

		cancel()
		synctest.Wait()
	})
}

// TestRunLeaseRenewalCancellationStopsWithNoLeak is the run-lease counterpart
// of TestTaskClaimRenewalCancellationStopsWithNoLeak.
func TestRunLeaseRenewalCancellationStopsWithNoLeak(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const runLeaseTTL = 20 * time.Second
		interval := batchLeaseRenewInterval(runLeaseTTL, 0)

		renewer := &mockRunLeaseRenewer{ownedCount: 1}
		w := &Worker{
			runLeaseRenewer: renewer,
			runLeaseTTL:     runLeaseTTL,
			runLeaseNodeID:  "node-a:9001",
		}

		ctx, cancel := context.WithCancel(context.Background())
		go w.runRunLeaseRenewal(ctx)

		time.Sleep(2*interval + interval/2)
		synctest.Wait()

		renewer.mu.Lock()
		before := renewer.renewCalls
		renewer.mu.Unlock()
		if before != 2 {
			t.Fatalf("expected 2 run-lease renewal calls before cancellation, got %d", before)
		}

		cancel()
		synctest.Wait() // waits for the goroutine to observe ctx.Done() and exit

		time.Sleep(5 * interval)
		renewer.mu.Lock()
		after := renewer.renewCalls
		renewer.mu.Unlock()
		if after != before {
			t.Fatalf("run-lease renewal continued after cancellation: got %d calls, want %d", after, before)
		}
	})
}
