package worker

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
)

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

	// Batched lease renewal.
	leaseRenewer       LeaseRenewer
	leaseTTL           time.Duration
	leaseRenewInterval time.Duration
	inFlightMu         sync.Mutex
	inFlight           map[uuid.UUID]*inFlightClaim
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
	// Start the batched lease renewal goroutine only when configured. A zero
	// leaseTTL would cause renewLeasesNow to set claim_expires_at = now on every
	// tick, immediately expiring all in-flight leases — so refuse to start the
	// goroutine in that case.
	if w.leaseRenewer != nil && w.leaseTTL > 0 && w.leaseRenewInterval > 0 {
		go w.runLeaseRenewal(ctx)
	}

	for {
		select {
		case <-ctx.Done():
			w.pool.Wait()
			return nil
		default:
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
			if sleepErr := waitForWork(ctx, w.wakeups, w.pollInterval); sleepErr != nil {
				w.pool.Wait()
				return nil
			}
			continue
		}

		// Register the claim before submitting so the renewal ticker can see it
		// as soon as the goroutine is alive, even before execution starts.
		w.trackInFlight(task)

		if err := w.pool.Submit(ctx, func() {
			defer w.untrackInFlight(task.ID)
			w.executor(ctx, task)
		}); err != nil {
			// Submit failed (context cancelled); undo the registration.
			w.untrackInFlight(task.ID)
			if ctx.Err() != nil {
				w.pool.Wait()
				return nil
			}
			return err
		}
	}
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

func waitForWork(ctx context.Context, wakeups <-chan struct{}, d time.Duration) error {
	if wakeups == nil {
		return sleepWithContext(ctx, d)
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-wakeups:
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
