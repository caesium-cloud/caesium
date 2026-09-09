package job

import (
	"context"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
)

// The run-cancel registry is the seam between "the run row says cancelled" and
// "the container the run started is gone".
//
// Cancelling a run (CancelRun, and the concurrency `replace` admission, which
// share cancelRunTx) writes every non-terminal task row `cancelled` and
// publishes TypeRunCancelled. In LOCAL execution mode nothing acted on that:
// the DAG loop runs on a context derived from context.Background() in whichever
// detached goroutine kicked the run off, so the container of the task that was
// mid-flight kept running to completion, wrote its logs, and — before PR #275's
// terminal guards — resurrected the cancelled row. The guards stopped the row
// lying; the container was still burning a slot, a rate-limit token and
// whatever the step actually does to the outside world.
//
// The registry closes that gap without teaching the store about executors:
// every detached run context is DERIVED from a registered cancellable context,
// and one subscriber turns the event into a cancel. internal/run stays
// untouched, which is what keeps this change and A1's store change disjoint.
//
// The map is keyed by run id and holds a SET of cancel funcs per run, not one:
// a run can legitimately have more than one live engine at a time (a partition
// retry starts a replacement engine against the same run id while the previous
// one is still draining its shutdown window), and a cancel must reach all of
// them.
type cancelRegistry struct {
	mu   sync.Mutex
	next uint64
	runs map[uuid.UUID]map[uint64]cancelEntry
}

// cancelEntry keeps the derived context beside its cancel func so the
// reconciliation sweep can tell an entry it must cancel from one the event
// already cancelled and whose engine is merely still draining. Without the
// context there is no way to ask "was this already stopped?", and the sweep
// would report every ordinary cancel as a lost event.
type cancelEntry struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func newCancelRegistry() *cancelRegistry {
	return &cancelRegistry{runs: make(map[uuid.UUID]map[uint64]cancelEntry)}
}

// register derives a cancellable context for one engine driving runID and
// returns it with a release func. release is idempotent and MUST be called when
// the engine returns (it cancels the derived context — so it also satisfies
// `go vet`'s lostcancel — and drops the registry entry, which is what stops the
// map growing for the lifetime of the process).
//
// A nil run id is not an error: the caller gets the parent context and a no-op
// release, so a code path that has no run to cancel is unchanged.
func (r *cancelRegistry) register(parent context.Context, runID uuid.UUID) (context.Context, func()) {
	if parent == nil {
		parent = context.Background()
	}
	if runID == uuid.Nil {
		return parent, func() {}
	}

	ctx, cancel := context.WithCancel(parent)

	r.mu.Lock()
	r.next++
	token := r.next
	if r.runs[runID] == nil {
		r.runs[runID] = make(map[uint64]cancelEntry, 1)
	}
	r.runs[runID][token] = cancelEntry{ctx: ctx, cancel: cancel}
	r.mu.Unlock()

	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			r.mu.Lock()
			if entries, ok := r.runs[runID]; ok {
				delete(entries, token)
				if len(entries) == 0 {
					delete(r.runs, runID)
				}
			}
			r.mu.Unlock()
			cancel()
		})
	}
}

// cancel cancels every context registered for runID and reports how many were
// cancelled. Zero is the ordinary case for a run this node is not executing
// (a distributed worker's containers are reached through the claim-loss path in
// internal/worker instead), so it is not an error.
//
// The entries are left in the map for their own release funcs to remove: a
// cancelled engine is still running its shutdown path and its release is the
// only thing that knows when that is over.
func (r *cancelRegistry) cancel(runID uuid.UUID) int {
	return r.cancelWhere(runID, nil)
}

// cancelLive is cancel restricted to contexts that are not already cancelled,
// and it reports how many those were.
//
// It is the reconciliation sweep's cancel, and the restriction is the whole
// difference between a useful signal and a noisy one. cancel() deliberately
// leaves entries in the map for their own release funcs to remove, so for a tick
// or two after an ORDINARY cancel the sweep still sees the run — cancelled row,
// registered entries, everything the "the event was lost" branch looks for.
// Counting those would make caesium_run_cancel_reconciled_total increment on
// every routine cancellation and mean nothing. Asking whether the context is
// still live answers the question that actually matters: did anything else
// already stop this?
func (r *cancelRegistry) cancelLive(runID uuid.UUID) int {
	return r.cancelWhere(runID, func(e cancelEntry) bool { return e.ctx.Err() == nil })
}

func (r *cancelRegistry) cancelWhere(runID uuid.UUID, match func(cancelEntry) bool) int {
	if runID == uuid.Nil {
		return 0
	}
	r.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(r.runs[runID]))
	for _, entry := range r.runs[runID] {
		if match != nil && !match(entry) {
			continue
		}
		cancels = append(cancels, entry.cancel)
	}
	r.mu.Unlock()

	for _, c := range cancels {
		c()
	}
	return len(cancels)
}

// tracked reports how many contexts are registered for runID. Test seam.
func (r *cancelRegistry) tracked(runID uuid.UUID) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runs[runID])
}

// registeredRunIDs snapshots the run ids that currently hold at least one
// registry entry. It is the reconciliation sweep's input: the set is bounded by
// the runs THIS node is executing, so it is small enough for the sweep to ask
// the store about all of them in one statement.
func (r *cancelRegistry) registeredRunIDs() []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]uuid.UUID, 0, len(r.runs))
	for runID := range r.runs {
		ids = append(ids, runID)
	}
	return ids
}

// defaultCancelRegistry is process-wide on purpose: the goroutine that starts a
// run (an HTTP controller, the backfill driver, a partition-retry replacement)
// and the subscriber that observes TypeRunCancelled have no other shared
// object, and threading one through every kickoff site would be a wider change
// than the fix.
var defaultCancelRegistry = newCancelRegistry()

// RegisterRunCancel derives the cancellable context a detached run must execute
// under. EVERY `run.WithContext(context.Background(), runID)` kickoff site
// wraps its parent with this call; missing one leaves that entry point's runs
// uncancellable, which is the bug in the first place.
//
//	ctx, release := job.RegisterRunCancel(context.Background(), r.ID)
//	defer release()
//	job.New(...).Run(runstorage.WithContext(ctx, r.ID))
func RegisterRunCancel(parent context.Context, runID uuid.UUID) (context.Context, func()) {
	return defaultCancelRegistry.register(parent, runID)
}

// CancelRunContexts cancels every in-process run context registered for runID
// and reports how many were cancelled.
func CancelRunContexts(runID uuid.UUID) int {
	return defaultCancelRegistry.cancel(runID)
}

// SubscribeRunCancellations wires the registry to the in-process event bus:
// TypeRunCancelled — published by both run.Store.CancelRun and the concurrency
// `replace` admission, which share cancelRunTx — cancels the run's registered
// contexts, and the local executor's taskCtx.Done() branch then force-stops the
// container (internal/job/job.go).
//
// It returns as soon as the subscription is established; the pump runs until
// ctx is done. A bus that refuses the subscription is logged and ignored rather
// than fatal: a server that cannot cancel containers is still a server that
// runs jobs.
func SubscribeRunCancellations(ctx context.Context, bus event.Bus) {
	if bus == nil {
		return
	}
	events, err := bus.Subscribe(ctx, event.Filter{
		Types:             []event.Type{event.TypeRunCancelled},
		IncludeQuarantine: true,
	})
	if err != nil {
		log.Error("run cancel registry: failed to subscribe to run cancellations", "error", err)
		return
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-events:
				if !ok {
					return
				}
				if n := CancelRunContexts(evt.RunID); n > 0 {
					log.Info("cancelling in-flight local run contexts", "run_id", evt.RunID, "contexts", n)
				}
			}
		}
	}()
}

// CancelledRunLookup is the single question the reconciliation sweep asks the
// run store: of these run ids, which are already `cancelled`. *run.Store
// satisfies it (internal/run/store.go, CancelledRunIDs), and keeping it an
// interface here is what lets the sweep be unit-tested without a database and
// keeps internal/job from growing a second dependency on the store's shape.
type CancelledRunLookup interface {
	CancelledRunIDs(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error)
}

// cancelReconciler closes the one hole SubscribeRunCancellations cannot see.
//
// The subscriber is the ONLY thing that turns a cancellation into a stopped
// container in local mode, and it learns about cancellations from exactly one
// message on the in-process bus. That bus does not queue: Publish's send is a
// non-blocking select, and a subscriber whose buffer is momentarily full simply
// loses the event (internal/event/bus.go increments
// caesium_event_bus_dropped_total and logs a warning). One lost run_cancelled
// therefore orphans that run's container FOREVER — the row reads `cancelled`,
// the container runs to completion holding a slot, a rate-limit token, and
// whatever the step does to the outside world. That is the exact failure A3 was
// built to close, arriving through a different door.
//
// So the event is treated as an optimisation, not as the source of truth: the
// store row is the truth, and this sweep re-derives the answer from it on a low
// frequency. The event still does the fast path (sub-millisecond); the sweep
// bounds the damage of losing one to a single interval.
//
// Only `cancelled` is acted on, which is a deliberate reading of the run status
// enum (internal/run/store.go) rather than "every terminal status":
//
//   - `cancelled` is the only status that MEANS "stop what you are doing", and
//     the only one written by a path (cancelRunTx, shared by CancelRun and the
//     concurrency `replace` admission) whose single event can go missing.
//   - `succeeded`/`failed` are written by the engine's OWN completion defer,
//     which then keeps using the run context to resolve pending partition
//     retries and hand the run off before release() finally cancels it. A sweep
//     tick landing in that window would abort a correct completion — a new bug,
//     not a fix — and by then there is no container left to stop anyway.
//   - `skipped` is a run created terminal on arrival by the dataset-hold
//     admission gate. It never starts an engine, so it can never have a
//     registered context to cancel.
//
// The sweep cancels through cancelLive, not cancel, so it acts on — and reports
// — only contexts that nothing else has already stopped. Entries linger in the
// registry until their engine releases them, so a run the EVENT cancelled is
// still in the sweep's input for a tick or two afterwards; treating those as
// work would make the metric fire on every ordinary cancellation and say
// nothing. It also makes the sweep naturally idempotent: having cancelled a run
// once, later ticks find nothing live and stay silent.
type cancelReconciler struct {
	registry *cancelRegistry
	lookup   CancelledRunLookup
}

func newCancelReconciler(registry *cancelRegistry, lookup CancelledRunLookup) *cancelReconciler {
	return &cancelReconciler{registry: registry, lookup: lookup}
}

// reconcile runs one sweep and returns how many contexts it cancelled. A store
// error is logged and swallowed: the next tick asks again, and a sweep that
// gave up permanently on a transient read failure would be worse than the bug
// it exists to fix.
func (c *cancelReconciler) reconcile(ctx context.Context) int {
	metrics.RunCancelReconcileSweepsTotal.Inc()

	ids := c.registry.registeredRunIDs()
	if len(ids) == 0 {
		return 0
	}

	// One statement for the whole registered set, not one per run: the sweep
	// runs forever on a timer, so a per-run Get would be a permanent
	// N-statements-per-tick tax on the single dqlite writer's connection.
	cancelled, err := c.lookup.CancelledRunIDs(ctx, ids)
	if err != nil {
		log.Error("run cancel reconciliation: failed to read run statuses", "runs", len(ids), "error", err)
		return 0
	}

	total := 0
	for _, runID := range cancelled {
		n := c.registry.cancelLive(runID)
		if n == 0 {
			continue
		}
		total += n
		metrics.RunCancelReconciledTotal.Add(float64(n))
		log.Warn("cancelling in-flight local run contexts by reconciliation: the run is cancelled but no run_cancelled event reached the registry",
			"run_id", runID, "contexts", n)
	}
	return total
}

// StartRunCancelReconciler runs the reconciliation sweep until ctx is done.
//
// It is the durability half of SubscribeRunCancellations and MUST be started
// beside it (cmd/start/start.go): the subscriber makes a cancel fast, this makes
// it certain. A non-positive interval disables the sweep, which is an operator
// escape hatch, not a default — with it off, a dropped run_cancelled is again
// permanent.
//
// It is deliberately NOT leader-gated, unlike the other background sweeps in
// cmd/start. The registry holds only the contexts of runs THIS process is
// executing, so there is nothing a leader could reconcile on another node's
// behalf — every node has to sweep its own.
//
// The goroutine owns nothing but its ticker and returns on ctx.Done(), so the
// server's shutdown context reaps it.
func StartRunCancelReconciler(ctx context.Context, lookup CancelledRunLookup, interval time.Duration) {
	if lookup == nil {
		return
	}
	if interval <= 0 {
		log.Warn("run cancel reconciliation is disabled; a dropped run_cancelled event will orphan a local container permanently",
			"interval", interval)
		return
	}

	reconciler := newCancelReconciler(defaultCancelRegistry, lookup)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				reconciler.reconcile(ctx)
			}
		}
	}()
}
