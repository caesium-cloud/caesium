package worker

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/dispatch"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
)

// RunLeaseRenewer is implemented by the run.LeaseStore and used by the
// worker's run-lease renewal goroutine.
type RunLeaseRenewer interface {
	// RenewOwnedLeases extends lease_expires_at in a single UPDATE for every
	// non-expired lease owned by ownerNode. Returns the number of rows
	// renewed, which is also the count of currently owned, non-expired runs.
	RenewOwnedLeases(ctx context.Context, ownerNode string, newExpiresAt time.Time) (int64, error)
}

const (
	defaultPollInterval    = 15 * time.Second
	defaultReclaimInterval = 30 * time.Second

	// defaultLeaseRenewDivisor is the fraction of lease_ttl used as the
	// renewal interval when no explicit interval is configured.  ttl/4 gives
	// three full renewal cycles before a lease would expire.
	defaultLeaseRenewDivisor = 4
)

type TaskClaimer interface {
	ClaimNext(ctx context.Context) (*models.TaskRun, error)
}

type TaskExecutor func(ctx context.Context, task *models.TaskRun)

type ExpiredReclaimer interface {
	ReclaimExpired(ctx context.Context) error
}

type ReclaimGate interface {
	CanReclaim(ctx context.Context) (bool, error)
}

type ReclaimGateFunc func(context.Context) (bool, error)

func (f ReclaimGateFunc) CanReclaim(ctx context.Context) (bool, error) {
	return f(ctx)
}

// LeaseRenewer is implemented by any component that can issue a single batched
// UPDATE extending claim_expires_at for a set of in-flight task runs. Returns
// the number of rows actually updated; useful for both metric accuracy and
// detecting concurrent claim reassignment.
type LeaseRenewer interface {
	RenewLeases(ctx context.Context, nodeID string, ids []uuid.UUID, newExpiresAt time.Time) (int64, error)
}

// inFlightClaim records the minimal state needed to decide whether renewal is
// required for a single in-flight task run.
type inFlightClaim struct {
	claimedBy      string
	claimExpiresAt time.Time
}

type Worker struct {
	claimer         TaskClaimer
	pool            *Pool
	pollInterval    time.Duration
	reclaimInterval time.Duration
	lastReclaim     time.Time
	reclaimGate     ReclaimGate
	executor        TaskExecutor
	wakeups         <-chan struct{}

	// Batched task-claim lease renewal.
	leaseRenewer       LeaseRenewer
	leaseTTL           time.Duration
	leaseRenewInterval time.Duration
	inFlightMu         sync.Mutex
	inFlight           map[uuid.UUID]*inFlightClaim

	// Batched run-lease renewal (Phase 2 run-owner mode).
	runLeaseRenewer RunLeaseRenewer
	runLeaseTTL     time.Duration
	runLeaseNodeID  string

	// Inbound dispatched tasks (Phase 2 run-owner push path).  HandleDispatch
	// hands accepted tasks here via SubmitDispatched; the Run loop drains them
	// alongside ClaimNext'd tasks and submits each onto the SAME w.pool so the
	// pool size and backpressure are unified across both paths.  nil until
	// WithInboundDispatch is called (i.e. owner mode off → byte-identical Phase 1).
	inbound chan inboundTask
	// inboundNotify is a wake-only signal poked by SubmitDispatched so the Run
	// loop's idle wait returns promptly when a dispatched task arrives, instead
	// of waiting a full poll interval.  Buffered size 1; a missed poke (buffer
	// already full) is fine because the loop re-checks inbound on every pass.
	inboundNotify chan struct{}
	// completionToken is the bearer token the owner sink uses when POSTing a
	// dispatched task's completion back to the owner's /internal/complete.
	completionToken string
}

// inboundTask pairs a dispatched task with the owner metadata the executor
// needs to route its completion back to the owner.
type inboundTask struct {
	task *models.TaskRun
	meta dispatchMeta
}

func NewWorker(claimer TaskClaimer, pool *Pool, pollInterval time.Duration, executor TaskExecutor) *Worker {
	if claimer == nil {
		panic("worker requires task claimer")
	}
	if pool == nil {
		pool = NewPool(1)
	}
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}
	if executor == nil {
		executor = func(context.Context, *models.TaskRun) {}
	}

	return &Worker{
		claimer:         claimer,
		pool:            pool,
		pollInterval:    pollInterval,
		reclaimInterval: defaultReclaimInterval,
		lastReclaim:     initialLastReclaim(time.Now(), defaultReclaimInterval),
		executor:        executor,
		inFlight:        make(map[uuid.UUID]*inFlightClaim),
	}
}

func (w *Worker) WithWakeups(ch <-chan struct{}) *Worker {
	w.wakeups = ch
	return w
}

func (w *Worker) WithReclaimGate(gate ReclaimGate) *Worker {
	w.reclaimGate = gate
	return w
}

func (w *Worker) WithReclaimInterval(interval time.Duration) *Worker {
	if interval <= 0 {
		interval = defaultReclaimInterval
	}
	w.reclaimInterval = interval
	w.lastReclaim = initialLastReclaim(time.Now(), interval)
	return w
}

// WithLeaseRenewal configures per-node batched lease renewal.
//
//   - renewer issues a single UPDATE for all in-flight claims at once.
//   - leaseTTL is the configured claim TTL; it drives the renewal cadence and
//     the skip-when-not-needed threshold.
//   - renewInterval is the override interval; pass 0 to use leaseTTL/4.
func (w *Worker) WithLeaseRenewal(renewer LeaseRenewer, leaseTTL, renewInterval time.Duration) *Worker {
	w.leaseRenewer = renewer
	w.leaseTTL = leaseTTL
	w.leaseRenewInterval = batchLeaseRenewInterval(leaseTTL, renewInterval)
	return w
}

// WithRunLeaseRenewal configures per-node batched run-lease renewal for
// Phase 2 run-owner mode.  Piggybacked on the same ticker cadence as task
// claim renewals (leaseTTL/4).  nodeID is the CAESIUM_NODE_ADDRESS value
// that identifies this node in run_leases.owner_node.
func (w *Worker) WithRunLeaseRenewal(renewer RunLeaseRenewer, leaseTTL time.Duration, nodeID string) *Worker {
	w.runLeaseRenewer = renewer
	w.runLeaseTTL = leaseTTL
	w.runLeaseNodeID = nodeID
	return w
}

// WithInboundDispatch enables the Phase 2 run-owner push path: the worker
// accepts dispatched tasks via SubmitDispatched and drains them onto the same
// execution pool as ClaimNext'd tasks.  completionToken is the bearer token the
// worker presents when reporting a dispatched task's completion back to the
// owner's /internal/complete (the CAESIUM_INTERNAL_WAKEUP_TOKEN).
//
// The buffer is sized to the pool size (floored at 1): it holds at most one
// pool's worth of tasks waiting for a slot, which lets the worker absorb a
// dispatch burst without blocking the Run loop's drain, while bounding memory
// and keeping backpressure unified with the pool — when the buffer fills,
// SubmitDispatched rejects and the owner re-dispatches.  Without an explicit
// call this stays nil and the worker behaves byte-identically to Phase 1.
func (w *Worker) WithInboundDispatch(completionToken string) *Worker {
	size := 1
	if w.pool != nil && cap(w.pool.sem) > size {
		size = cap(w.pool.sem)
	}
	w.inbound = make(chan inboundTask, size)
	w.inboundNotify = make(chan struct{}, 1)
	w.completionToken = completionToken
	return w
}

// ErrInboundFull is returned by SubmitDispatched when the inbound buffer is at
// capacity (the worker is saturated).  The dispatch handler treats this as a
// rejectable condition and rolls the claim back so the owner re-dispatches.
var ErrInboundFull = errors.New("worker: inbound dispatch buffer full")

// ErrWorkerNotAccepting is returned by SubmitDispatched when the worker is not
// configured to accept dispatched tasks (WithInboundDispatch was never called).
var ErrWorkerNotAccepting = errors.New("worker: not accepting dispatched tasks")

// SubmitDispatched enqueues a dispatched task for execution on the worker's
// shared pool.  It is non-blocking: it returns ErrInboundFull when the inbound
// buffer is full and ErrWorkerNotAccepting when the inbound path is disabled.
// *Worker implements dispatch.WorkerSubmitter via this method, so the dispatch
// handler can hand it accepted tasks directly (no adapter needed).
//
// The non-blocking contract is deliberate: HandleDispatch runs on an HTTP
// request goroutine and must not block on a full pool.  A full buffer surfaces
// as a 409 the owner retries, rather than holding the dispatch RPC open.
func (w *Worker) SubmitDispatched(d dispatch.InboundDispatch) error {
	if w.inbound == nil {
		return ErrWorkerNotAccepting
	}
	if d.Task == nil {
		return errors.New("worker: dispatched task is nil")
	}
	// The completion bearer token is the internal wakeup token the worker holds
	// (from WithInboundDispatch); it is NOT carried in the dispatch envelope so
	// it never travels owner→worker on the wire needlessly.
	meta := dispatchMeta{
		OwnerBaseURL:    d.OwnerBaseURL,
		Token:           w.completionToken,
		WorkerNode:      d.WorkerNode,
		OwnerGeneration: d.OwnerGeneration,
		Attempt:         d.Attempt,
	}
	select {
	case w.inbound <- inboundTask{task: d.Task, meta: meta}:
		// Poke the wake signal (non-blocking; a full notify buffer means the
		// loop is already about to drain).
		select {
		case w.inboundNotify <- struct{}{}:
		default:
		}
		return nil
	default:
		return ErrInboundFull
	}
}

// batchLeaseRenewInterval derives the per-node batched renewal interval.
// If override > 0 it is used directly; otherwise leaseTTL/4 is used (minimum 1s).
func batchLeaseRenewInterval(leaseTTL, override time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	if leaseTTL <= 0 {
		return time.Second
	}
	interval := leaseTTL / defaultLeaseRenewDivisor
	if interval < time.Second {
		return time.Second
	}
	return interval
}

func (w *Worker) Run(ctx context.Context) error {
	// Start the batched task-claim lease renewal goroutine only when configured.
	// A zero leaseTTL would cause renewLeasesNow to set claim_expires_at = now
	// on every tick, immediately expiring all in-flight leases — so refuse to
	// start the goroutine in that case.
	if w.leaseRenewer != nil && w.leaseTTL > 0 && w.leaseRenewInterval > 0 {
		go w.runLeaseRenewal(ctx)
	}

	// Start the run-lease renewal goroutine when Phase 2 owner mode is active.
	if w.runLeaseRenewer != nil && w.runLeaseTTL > 0 && w.runLeaseNodeID != "" {
		go w.runRunLeaseRenewal(ctx)
	}

	for {
		select {
		case <-ctx.Done():
			w.pool.Wait()
			return nil
		default:
		}

		// Drain any dispatched (push-path) tasks first so the run-owner path
		// shares the same pool and backpressure as ClaimNext'd (pull-path) tasks.
		if err := w.drainInbound(ctx); err != nil {
			w.pool.Wait()
			return nil
		}

		w.reclaimIfDue(ctx)

		task, err := w.claimer.ClaimNext(ctx)
		if err != nil {
			if ctx.Err() != nil {
				w.pool.Wait()
				return nil
			}
			log.Error("failed to claim next task", "error", err)
		}

		if err != nil || task == nil {
			// No pull-path work: wait for a wakeup, a dispatched-task arrival, or
			// the poll interval — whichever comes first.  The inboundNotify wake
			// means a dispatched task is picked up promptly instead of after a
			// full poll interval.
			if sleepErr := waitForWork(ctx, w.wakeups, w.inboundNotify, w.pollInterval); sleepErr != nil {
				w.pool.Wait()
				return nil
			}
			continue
		}

		// ClaimNext'd task: execute with the plain context (local sink path).
		if err := w.submitToPool(ctx, ctx, task); err != nil {
			if ctx.Err() != nil {
				w.pool.Wait()
				return nil
			}
			return err
		}
	}
}

// drainInbound submits every currently-buffered dispatched task onto the shared
// pool.  It pulls non-blocking until the channel is momentarily empty so a
// burst of dispatches is absorbed in one pass.  Each dispatched task executes
// with a context carrying its owner metadata so the executor selects the
// owner-routed completion sink.  Returns a non-nil error only when the context
// was cancelled mid-submit (pool.Submit blocks on a full pool until a slot
// frees or ctx is done).
func (w *Worker) drainInbound(ctx context.Context) error {
	if w.inbound == nil {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case in := <-w.inbound:
			execCtx := withDispatchMeta(ctx, in.meta)
			if err := w.submitToPool(execCtx, ctx, in.task); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

// submitToPool registers a task as in-flight and submits its execution onto the
// pool.  execCtx is the context passed to the executor (carries dispatch
// metadata for push-path tasks); submitCtx governs the pool.Submit blocking
// (the worker's lifecycle context).  On a failed submit the in-flight
// registration is undone so the lease-renewal ticker doesn't track a task that
// never ran.
func (w *Worker) submitToPool(execCtx, submitCtx context.Context, task *models.TaskRun) error {
	// Register the claim before submitting so the renewal ticker can see it as
	// soon as the goroutine is alive, even before execution starts.
	w.trackInFlight(task)
	if err := w.pool.Submit(submitCtx, func() {
		defer w.untrackInFlight(task.ID)
		w.executor(execCtx, task)
	}); err != nil {
		w.untrackInFlight(task.ID)
		return err
	}
	return nil
}

// trackInFlight registers a task run as in-flight for lease renewal purposes.
func (w *Worker) trackInFlight(task *models.TaskRun) {
	if task == nil {
		return
	}
	claim := &inFlightClaim{
		claimedBy: task.ClaimedBy,
	}
	if task.ClaimExpiresAt != nil {
		claim.claimExpiresAt = *task.ClaimExpiresAt
	}
	w.inFlightMu.Lock()
	w.inFlight[task.ID] = claim
	w.inFlightMu.Unlock()
}

// untrackInFlight removes a task run from the in-flight set when execution ends.
func (w *Worker) untrackInFlight(id uuid.UUID) {
	w.inFlightMu.Lock()
	delete(w.inFlight, id)
	w.inFlightMu.Unlock()
}

// runLeaseRenewal is the background goroutine that fires the per-node batched
// lease renewal on a fixed cadence.
func (w *Worker) runLeaseRenewal(ctx context.Context) {
	ticker := time.NewTicker(w.leaseRenewInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.renewLeasesNow(ctx)
		}
	}
}

// renewLeasesNow groups in-flight claims by their claimedBy node, skips the
// UPDATE for groups where no claim is within lease_ttl/2 of expiry, and
// otherwise issues one batched UPDATE per node. In-memory expiry is updated
// only for IDs whose DB row was actually renewed — so a claim reassigned to
// another node between the renewal decision and the write keeps its true
// expiry in memory, matching what the DB now holds.
func (w *Worker) renewLeasesNow(ctx context.Context) {
	if w.leaseRenewer == nil || w.leaseTTL <= 0 {
		return
	}

	now := time.Now().UTC()
	halfTTL := w.leaseTTL / 2

	// Snapshot the in-flight set grouped by claimedBy. A worker normally tracks
	// claims for a single node, but a stale or cross-node claim should not
	// contaminate another node's UPDATE.
	w.inFlightMu.Lock()
	byNode := make(map[string][]uuid.UUID)
	needsRenewal := false
	for id, claim := range w.inFlight {
		byNode[claim.claimedBy] = append(byNode[claim.claimedBy], id)
		if !needsRenewal && (claim.claimExpiresAt.IsZero() || claim.claimExpiresAt.Sub(now) <= halfTTL) {
			needsRenewal = true
		}
	}
	w.inFlightMu.Unlock()

	if !needsRenewal {
		return
	}

	newExpiresAt := now.Add(w.leaseTTL)
	for nodeID, ids := range byNode {
		if len(ids) == 0 {
			continue
		}
		rowsAffected, err := w.leaseRenewer.RenewLeases(ctx, nodeID, ids, newExpiresAt)
		if err != nil {
			if ctx.Err() == nil {
				log.Error("failed to renew worker task leases", "node_id", nodeID, "count", len(ids), "error", err)
			}
			continue
		}
		if rowsAffected <= 0 {
			// Nothing was actually renewed (every claim was reassigned in the
			// window). Don't touch the counter or the in-memory expiries —
			// stale rows will surface on the next tick or expire naturally.
			continue
		}
		metrics.DBWritesTotal.WithLabelValues(metrics.DBWriteCategoryLeaseRenewal).Add(float64(rowsAffected))
		metrics.DBStatementsTotal.WithLabelValues(metrics.DBWriteCategoryLeaseRenewal).Inc()

		// Update the in-memory expiry only for the IDs we attempted to renew
		// AND whose claimedBy is still nodeID (the latter check guards against
		// a concurrent local reassignment between snapshot and write).
		w.inFlightMu.Lock()
		for _, id := range ids {
			if claim, ok := w.inFlight[id]; ok && claim.claimedBy == nodeID {
				claim.claimExpiresAt = newExpiresAt
			}
		}
		w.inFlightMu.Unlock()
	}
}

// runRunLeaseRenewal is the background goroutine that extends run_leases rows
// for every run owned by this node.  It piggybacks on the same leaseTTL/4
// cadence as the task-claim renewal ticker so the two renewal paths share
// tuning knobs.
func (w *Worker) runRunLeaseRenewal(ctx context.Context) {
	interval := batchLeaseRenewInterval(w.runLeaseTTL, 0)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.renewRunLeasesNow(ctx)
		}
	}
}

// renewRunLeasesNow extends lease_expires_at in a single UPDATE for every
// non-expired lease this node owns. The previous fetch-then-update pair has
// been collapsed to one round-trip; rowsAffected is the count of currently
// owned runs, which we publish to the gauge unconditionally (so the gauge
// resets to 0 cleanly when the owned set empties).
func (w *Worker) renewRunLeasesNow(ctx context.Context) {
	if w.runLeaseRenewer == nil || w.runLeaseTTL <= 0 || w.runLeaseNodeID == "" {
		return
	}

	newExpiresAt := time.Now().UTC().Add(w.runLeaseTTL)
	rowsAffected, err := w.runLeaseRenewer.RenewOwnedLeases(ctx, w.runLeaseNodeID, newExpiresAt)
	if err != nil {
		if ctx.Err() == nil {
			log.Error("run owner: failed to renew run leases",
				"node_id", w.runLeaseNodeID,
				"error", err,
			)
		}
		return
	}

	// Always publish the current owned-run count, even when zero, so the
	// gauge accurately reflects the state instead of holding the last
	// non-zero value indefinitely.
	// Control-plane node-load series: intentionally includes replay work and is
	// not a run-health input.
	metrics.RunLeasesOwned.Set(float64(rowsAffected))
	if rowsAffected > 0 {
		// Control-plane node-load series: intentionally includes replay work and
		// is not a run-health input.
		metrics.RunLeaseRenewalsTotal.Inc()
	}
}

func (w *Worker) reclaimIfDue(ctx context.Context) {
	reclaimer, ok := w.claimer.(ExpiredReclaimer)
	if !ok {
		return
	}
	if time.Since(w.lastReclaim) < w.reclaimInterval {
		return
	}

	w.lastReclaim = time.Now()
	if w.reclaimGate != nil {
		canReclaim, err := w.reclaimGate.CanReclaim(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("failed to evaluate reclaim leadership", "error", err)
			}
			return
		}
		if !canReclaim {
			return
		}
	}
	if err := reclaimer.ReclaimExpired(ctx); err != nil && ctx.Err() == nil {
		log.Error("failed to reclaim expired tasks", "error", err)
	}
}

func initialLastReclaim(now time.Time, interval time.Duration) time.Time {
	return now.Add(-interval).Add(randomReclaimOffset(interval))
}

func randomReclaimOffset(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(interval)))
}

// waitForWork blocks until there is plausibly work to do: a pull-path wakeup
// signal, a dispatched-task arrival notification, or the poll interval
// elapsing.  notify is a wake-only signal (buffered size 1, poked by
// SubmitDispatched); consuming it is harmless because the actual task sits
// safely in the inbound buffer and is drained by the next loop iteration.  A
// nil wakeups and nil notify (owner mode off, no wakeups) collapses to the
// pre-Phase-2 sleep behavior.
func waitForWork(ctx context.Context, wakeups <-chan struct{}, notify <-chan struct{}, d time.Duration) error {
	if wakeups == nil && notify == nil {
		return sleepWithContext(ctx, d)
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-wakeups:
		return nil
	case <-notify:
		return nil
	case <-timer.C:
		return nil
	}
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	// Add up to 20% jitter to the sleep duration to avoid synchronized polling
	var jitter time.Duration
	if maxJitter := int64(d) / 5; maxJitter > 0 {
		jitter = time.Duration(rand.Int64N(maxJitter))
	}
	timer := time.NewTimer(d + jitter)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
