package job

import (
	"context"
	"sync"

	"github.com/caesium-cloud/caesium/internal/event"
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
	runs map[uuid.UUID]map[uint64]context.CancelFunc
}

func newCancelRegistry() *cancelRegistry {
	return &cancelRegistry{runs: make(map[uuid.UUID]map[uint64]context.CancelFunc)}
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
		r.runs[runID] = make(map[uint64]context.CancelFunc, 1)
	}
	r.runs[runID][token] = cancel
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
	if runID == uuid.Nil {
		return 0
	}
	r.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(r.runs[runID]))
	for _, c := range r.runs[runID] {
		cancels = append(cancels, c)
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
