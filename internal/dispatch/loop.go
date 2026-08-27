// Package dispatch: dispatch loop — per-node goroutine that polls owned runs
// for ready tasks and pushes them to workers via PostDispatch.
//
// The loop runs only when CAESIUM_RUN_OWNER_ENABLED=true.  When disabled, the
// field in start.go stays nil and the system behaves byte-identically to Phase 1.
//
// Design decisions:
//   - Per-node (not per-run): one goroutine iterates all owned runs each tick;
//     no per-run goroutines are spawned.
//   - Round-robin peer selection for Phase A2: least-loaded requires a
//     worker-status RPC that doesn't exist yet.  The local node is included in
//     the rotation so single-node setups work.
//   - On PostDispatch returns false (network error or 409): leave the task
//     untouched (claimed_by="", status=pending) so ClaimNext recovery picks it up.
//   - Batch cap (CAESIUM_RUN_OWNER_DISPATCH_BATCH, default 64): prevents a huge
//     fan-out from stalling the tick loop.
//   - Skip-when-quiet: if no peers are discovered yet (cluster bootstrapping),
//     or no owned runs exist, exit the tick early without writing anything.

package dispatch

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/ratelimit"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// DispatchRejectionReason labels for caesium_dispatch_rejected_total.
const (
	DispatchReasonNetworkError       = "network_error"
	DispatchReasonWorkerRejected     = "worker_rejected"
	DispatchReasonNoPeers            = "no_peers"             // peer discovery returned empty list (bootstrap)
	DispatchReasonPeerDiscoveryError = "peer_discovery_error" // peer discovery RPC failed
	// DispatchReasonNoCapablePeer counts fan-out INSTANCES that could not be
	// dispatched because no peer in the rotation advertises
	// CapabilityInstanceIdentity — the rolling-deploy window where every live
	// peer still predates instance-addressed dispatch.  The instance is left on
	// the ready queue and retried next tick, so this is a stall signal, not a
	// loss signal.
	DispatchReasonNoCapablePeer = "no_instance_capable_peer"
)

// Peer-capability cache TTLs.  A positive answer is stable for the life of a
// process, so it is cached long; a negative answer is re-probed quickly because
// it is exactly what a rolling deploy is in the middle of changing.
const (
	peerCapPositiveTTL = 5 * time.Minute
	peerCapNegativeTTL = 10 * time.Second
)

// PeerLister provides the current set of dispatch-eligible peer node addresses.
// The production implementation delegates to the dqlite cluster member list;
// tests inject a stub.  Addresses are returned as "host:dqlitePort" strings —
// the same format as CAESIUM_NODE_ADDRESS / dqlite.Cluster results.
type PeerLister interface {
	DispatchPeers(ctx context.Context) ([]string, error)
}

// PeerListerFunc is a function-valued implementation of PeerLister.
type PeerListerFunc func(context.Context) ([]string, error)

func (f PeerListerFunc) DispatchPeers(ctx context.Context) ([]string, error) {
	return f(ctx)
}

// OwnerReader provides run-lease ownership queries used by the dispatch loop.
//
// OwnedRunsWithGenerations returns owned runIDs mapped to their current
// lease generation in a single query — used per-tick to avoid an N+1
// GetLease pattern as the owned set grows.
type OwnerReader interface {
	OwnedRunsWithGenerations(ctx context.Context, ownerNode string) (map[uuid.UUID]int64, error)
	// AcquireExpiredLeases takes over leases whose owner let them expire,
	// reassigning them to ownerNode with an incremented generation.  Used by the
	// in-memory failover sweep so a peer recovers a dead owner's runs.
	AcquireExpiredLeases(ctx context.Context, newOwner string, ttl time.Duration) (int64, error)
}

// peer pairs a peer's canonical node identity (host:dqlitePort — matches the
// receiving handler's nodeID, derived from CAESIUM_NODE_ADDRESS) with the HTTP
// base URL the dispatch loop POSTs to (http://host:apiPort). The receiving
// handler validates `req.WorkerNode == h.nodeID`; using the dqlite-port
// identity here is what makes that validation pass.
type peer struct {
	nodeID  string
	baseURL string
}

// TaskPendingReader provides pending-task queries used by the dispatch loop.
type TaskPendingReader interface {
	PendingTasksForDispatch(ctx context.Context, runID uuid.UUID, limit int) ([]models.TaskRun, error)
}

// RateLimiter consumes durable resource tokens before a task is dispatched.
type RateLimiter interface {
	Acquire(ctx context.Context, resource string, units, limit int, window time.Duration) (bool, error)
}

type rateLimitTaskUpdater interface {
	RateLimitTask(ctx context.Context, runID, taskID uuid.UUID, retryAfter time.Time) error
}

// expiredClaimReclaimer is the optional store capability the SQL dispatch lane
// uses to return a dead worker's in-flight rows to the dispatchable pool.
// Declared as an optional interface, like rateLimitTaskUpdater above, so the
// production *run.Store satisfies it with no wiring change and a test's minimal
// stub store simply opts out.
type expiredClaimReclaimer interface {
	ReclaimOwnerExpiredClaims(runID uuid.UUID, ownerGeneration int64) ([]models.TaskRun, error)
}

// DispatchLoopConfig holds all parameters for the dispatch loop goroutine.
type DispatchLoopConfig struct {
	// NodeID is this node's canonical address (CAESIUM_NODE_ADDRESS).  Used
	// as the identity for OwnedRuns and included in the round-robin peer list.
	NodeID string
	// APIPort is the HTTP API port (CAESIUM_PORT).  Used to build the dispatch
	// URL from peer node addresses when InternalPort is unset (tests / non-mTLS).
	APIPort int
	// InternalPort is the dedicated internal mTLS listener port
	// (CAESIUM_INTERNAL_PORT).  When > 0, peer and owner base URLs are built as
	// https://host:InternalPort so dispatch/complete traffic flows over the
	// mutually-authenticated internal listener instead of the public API port.
	InternalPort int
	// Token is the CAESIUM_INTERNAL_WAKEUP_TOKEN bearer token.
	Token string
	// Interval is the polling tick interval (CAESIUM_RUN_OWNER_DISPATCH_INTERVAL).
	Interval time.Duration
	// BatchSize caps the number of tasks dispatched per tick per run
	// (CAESIUM_RUN_OWNER_DISPATCH_BATCH).
	BatchSize int
	// Deadline is added to time.Now() to produce the DispatchRequest.Deadline
	// (CAESIUM_RUN_OWNER_DISPATCH_DEADLINE).
	Deadline time.Duration
	// LeaseTTL is the run-lease TTL (CAESIUM_RUN_LEASE_TTL), used as the new
	// expiry when this node takes over an expired lease in the failover sweep.
	LeaseTTL time.Duration
	// LeaseStore provides ownership queries.
	LeaseStore OwnerReader
	// Store provides pending-task queries.
	Store TaskPendingReader
	// RateLimitDB is the catalog DB used to resolve persisted task/job
	// rate-limit metadata. Nil disables rate limiting for tests.
	RateLimitDB *gorm.DB
	// RateLimiter enforces declared resource limits before worker dispatch.
	RateLimiter RateLimiter
	// Peers resolves the current peer list.
	Peers PeerLister
	// PeerBaseURL maps a raw peer node address (host:dqlitePort) to the HTTP
	// base URL the dispatch loop POSTs to (http://host:apiPort). Optional;
	// tests override it to route multiple distinct peer node IDs to a single
	// mux server. Production leaves it nil and the loop falls back to the
	// default (build URL from APIPort).
	PeerBaseURL func(nodeAddr string) string
	// OwnerManager, when set (CAESIUM_RUN_OWNER_IN_MEMORY=true), is the source of
	// truth for ready tasks: the loop dispatches from the in-memory ready queue
	// and records dispatches/recoveries on it, instead of polling the DB for
	// pending tasks.  Nil keeps the proven B2 DB-poll path.
	OwnerManager *run.OwnerManager
}

// DispatchLoop is the per-node push-dispatch goroutine for Phase A2.
// Call Run(ctx) in a goroutine; it exits cleanly when ctx is cancelled.
type DispatchLoop struct {
	cfg     DispatchLoopConfig
	counter atomic.Uint64 // round-robin counter; used modulo peer count
	// ownerBaseURL is this node's own API base URL, stamped onto every
	// DispatchRequest.OwnerBaseURL so the receiving worker knows where to POST
	// its completion.  Computed once from NodeID + APIPort.
	ownerBaseURL string

	// benchedPeers circuit-breaks peers that fail a dispatch with a network
	// error (connection refused / timeout), keyed by nodeID → bench-expiry time.
	// Peer discovery returns the raw dqlite cluster membership, which keeps
	// listing a crashed node (with its old IP) until the cluster reconciles —
	// with ephemeral storage and dynamic pod IPs that can linger indefinitely.
	// Benching such a peer keeps the round-robin from burning a full
	// dispatchPostTimeout on it every tick; the bench lapses after a cooldown so
	// a transiently-blipped peer recovers and a genuinely dead one costs only one
	// timeout per cooldown.  A 409 (worker_rejected) does NOT bench: that peer is
	// alive and merely declined.
	benchMu      sync.Mutex
	benchedPeers map[string]time.Time

	// In-memory owner mode does not poll PendingTasksForDispatch, so it needs a
	// local mirror of rate-limit retry-after times to avoid re-dispatch spin.
	rateLimitDelayMu sync.Mutex
	rateLimitDelays  map[uuid.UUID]map[uuid.UUID]time.Time

	// peerCaps caches each peer's advertised capability set so the gate costs one
	// probe per peer per TTL rather than one per dispatch.
	capsMu   sync.Mutex
	peerCaps map[string]peerCapEntry

	// lastReclaim throttles the SQL lane's expired-claim sweep to one query per
	// run per ownerReclaimInterval.  The in-memory lane keeps this per-run clock
	// on the OwnerManager, where it can also consult the owner's own lease
	// bookkeeping first; the SQL lane has no in-memory state to consult, so the
	// interval is the whole gate.
	reclaimMu   sync.Mutex
	lastReclaim map[uuid.UUID]time.Time
}

// ownerReclaimInterval is the floor between expired-claim sweeps for one run in
// the SQL lane.  It mirrors the OwnerManager's default so both lanes recover a
// dead worker's task on the same timescale.
const ownerReclaimInterval = 15 * time.Second

// peerCapEntry is one peer's cached capability answer and when it goes stale.
type peerCapEntry struct {
	instanceIdentity bool
	expiresAt        time.Time
}

// peerBenchCooldown is how long a peer stays benched after a network-error
// dispatch failure.  Comfortably larger than the dispatch tick interval (so a
// dead peer is skipped across many ticks) but short enough that a recovered or
// transiently-unreachable peer rejoins the rotation quickly.
const peerBenchCooldown = 10 * time.Second

// NewDispatchLoop constructs a DispatchLoop from cfg.
func NewDispatchLoop(cfg DispatchLoopConfig) *DispatchLoop {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 64
	}
	if cfg.Deadline <= 0 {
		cfg.Deadline = 5 * time.Minute
	}
	if cfg.APIPort <= 0 {
		cfg.APIPort = 8080
	}
	l := &DispatchLoop{
		cfg:             cfg,
		benchedPeers:    make(map[string]time.Time),
		rateLimitDelays: make(map[uuid.UUID]map[uuid.UUID]time.Time),
		peerCaps:        make(map[string]peerCapEntry),
		lastReclaim:     make(map[uuid.UUID]time.Time),
	}
	// Reuse the same nodeAddr→baseURL logic the peer list uses so the owner's
	// own base URL is built identically (and honors the PeerBaseURL test hook).
	l.ownerBaseURL = l.nodeAddrToBaseURL(cfg.NodeID)
	return l
}

// Run starts the polling loop.  It blocks until ctx is cancelled.
func (l *DispatchLoop) Run(ctx context.Context) {
	ticker := time.NewTicker(l.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.tick(ctx)
		}
	}
}

// tick executes one dispatch sweep: discover peers, find owned runs with ready
// tasks, and POST a DispatchRequest for each task up to BatchSize.
func (l *DispatchLoop) tick(ctx context.Context) {
	// 0. Failover sweep (in-memory mode only): take over any lease whose owner
	//    let it expire, so a dead owner's runs get a live owner that recovers and
	//    resumes them.  This runs FIRST, before peer discovery — taking over a
	//    lease is a catalog write independent of cluster membership, and peer
	//    discovery is exactly what fails during the dqlite quorum disruption an
	//    owner crash causes.  Gating takeover behind it would mean the very event
	//    that needs failover also blocks it.  In SQL mode, ClaimNext recovery
	//    handles this instead.
	if l.cfg.OwnerManager != nil {
		if n, takeErr := l.cfg.LeaseStore.AcquireExpiredLeases(ctx, l.cfg.NodeID, l.cfg.LeaseTTL); takeErr != nil {
			if ctx.Err() == nil {
				log.Warn("dispatch loop: expired-lease takeover failed", "error", takeErr)
			}
		} else if n > 0 {
			log.Info("dispatch loop: took over expired run leases", "count", n, "new_owner", l.cfg.NodeID)
		}
	}

	// 1. Discover peers (includes self).
	rawPeers, err := l.cfg.Peers.DispatchPeers(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Warn("dispatch loop: peer discovery failed", "error", err)
		// Distinct from no_peers (empty list = normal bootstrap) so dashboards
		// can alert on real RPC failures separately.
		// This run-set-wide control-plane metric has no per-task quarantine context.
		metrics.DispatchRejectedTotal.WithLabelValues(DispatchReasonPeerDiscoveryError).Inc()
		return
	}
	// Normalise peers to {nodeID, baseURL} pairs; include self in the rotation.
	peers := l.buildPeers(rawPeers)
	if len(peers) == 0 {
		// This run-set-wide control-plane metric has no per-task quarantine context.
		metrics.DispatchRejectedTotal.WithLabelValues(DispatchReasonNoPeers).Inc()
		return
	}
	// Drop peers currently benched for repeated network failures so the
	// round-robin doesn't keep selecting a node that has left the cluster but
	// still lingers in dqlite membership.  Falls back to the full list if every
	// peer is benched, so a cluster-wide blip never starves dispatch entirely.
	peers = l.healthyPeers(peers)

	// 2. Find runs this node owns AND their current generation in one query
	//    (avoids the N+1 GetLease pattern as the owned set grows).
	ownedRuns, err := l.cfg.LeaseStore.OwnedRunsWithGenerations(ctx, l.cfg.NodeID)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Warn("dispatch loop: OwnedRunsWithGenerations failed", "error", err)
		return
	}
	l.forgetUnownedReclaims(ownedRuns)
	if len(ownedRuns) == 0 {
		return // nothing to do
	}

	// 3. For each owned run, find ready tasks and dispatch them concurrently.
	for runID, generation := range ownedRuns {
		if ctx.Err() != nil {
			return
		}
		l.dispatchRun(ctx, runID, generation, peers)
	}
}

// dispatchRun dispatches up to BatchSize ready tasks for a single owned run.
// Each task's PostDispatch fires in a worker goroutine bounded by BatchSize/4
// (capped at 16) so slow or unreachable workers don't serialise the tick.
func (l *DispatchLoop) dispatchRun(ctx context.Context, runID uuid.UUID, generation int64, peers []peer) {
	// In-memory mode: dispatch from the owner's RunState ready queue rather than
	// polling the DB.  Adopt-or-recover the run lazily on first sight.
	if l.cfg.OwnerManager != nil {
		l.dispatchRunInMemory(ctx, runID, generation, peers)
		return
	}

	// Return any in-flight row whose worker died to the dispatchable pool BEFORE
	// polling, so a reclaimed task is re-dispatched on this same tick.
	//
	// The SQL lane needs this for exactly the reason the in-memory lane does:
	// Claimer.ReclaimExpired's live-lease guard skips rows belonging to a run
	// whose owner is alive, and PendingTasksForDispatch only ever returns
	// `pending` rows — so a row left `running` with a lapsed claim was invisible
	// to both and the task was never re-run.  Here the reset is the whole fix: a
	// row back at pending is picked up by the very next poll, with no in-memory
	// state to keep in step.
	l.reclaimExpiredClaims(runID, generation)

	tasks, err := l.cfg.Store.PendingTasksForDispatch(ctx, runID, l.cfg.BatchSize)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Warn("dispatch loop: PendingTasksForDispatch failed",
			"run_id", runID, "error", err)
		return
	}
	if len(tasks) == 0 {
		return
	}

	// Bound the per-tick concurrent dispatches so we don't fan out 64 goroutines
	// for every owned run. 16 is a soft cap that keeps slow workers from
	// stalling the loop while not requiring a full worker-pool abstraction.
	const maxConcurrent = 16
	concurrency := len(tasks)
	if concurrency > maxConcurrent {
		concurrency = maxConcurrent
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for i := range tasks {
		if ctx.Err() != nil {
			break
		}
		task := &tasks[i]

		// Pick a peer via round-robin (atomic counter is per-loop, so per-task
		// dispatch rotation is monotonic across runs and ticks).
		idx := l.counter.Add(1) - 1
		p := peers[idx%uint64(len(peers))]

		req := DispatchRequest{
			RunID:  runID,
			TaskID: task.TaskID,
			// PendingTasksForDispatch returns rows, and a fanned step has N rows
			// sharing one task_id; naming the row is what stops the worker from
			// having to disambiguate siblings.
			TaskRunID:       task.ID,
			OwnerGeneration: generation,
			Attempt:         task.Attempt,
			// nodeID matches the recipient's CAESIUM_NODE_ADDRESS so the
			// handler's `req.WorkerNode == h.nodeID` check passes.
			WorkerNode: p.nodeID,
			// OwnerBaseURL is this node's (the owner's) own API base URL; the
			// receiving worker POSTs its completion back here so the owner stays
			// the single writer for its run's hot rows.
			OwnerBaseURL: l.ownerBaseURL,
			Deadline:     time.Now().UTC().Add(l.cfg.Deadline),
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(p peer, req DispatchRequest) {
			defer wg.Done()
			defer func() { <-sem }()
			if ok := l.acquireRateLimit(ctx, runID, req.TaskID, req.TaskRunID); !ok {
				return
			}
			l.postOne(ctx, runID, p, req, task.Quarantine)
		}(p, req)
	}
	wg.Wait()
}

// dispatchRunInMemory dispatches a run's ready tasks from the owner's in-memory
// RunState.  It lazily adopts/recovers the run on first sight (Recover handles
// both a freshly-created run — no checkpoint, fresh state — and a takeover —
// replay from checkpoint + terminal tail, re-queuing lost in-flight work).
func (l *DispatchLoop) dispatchRunInMemory(ctx context.Context, runID uuid.UUID, generation int64, peers []peer) {
	mgr := l.cfg.OwnerManager
	if !mgr.Owns(runID) {
		if _, err := mgr.Recover(runID, generation); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warn("dispatch loop: owner recover failed", "run_id", runID, "error", err)
			return
		}
	}

	// Return any in-flight task whose worker died (its claim lease lapsed) to the
	// ready queue before reading it.  The claimer's reaper deliberately skips
	// rows of a live-owned run, so this is the only thing that recovers them
	// while the owner is healthy.  Cheap: the manager skips the query entirely
	// unless the run holds an in-flight lease that looks overdue AND its reap
	// interval has elapsed, so a quiet run costs nothing per tick.
	if requeued := mgr.ReclaimExpiredClaims(runID); len(requeued) > 0 {
		log.Warn("dispatch loop: re-queued tasks whose worker claim lease expired",
			"run_id", runID, "count", len(requeued))
	}

	ready := mgr.ReadyForDispatch(runID)
	if len(ready) == 0 {
		return
	}
	now := time.Now().UTC()
	filtered := ready[:0]
	for _, dt := range ready {
		// Rate-limit parking is per *row*: RateLimitTask stamps
		// rate_limit_retry_after on one instance, so one parked sibling must not
		// hold back the rest of its group.
		if l.rateLimitDelayed(runID, dt.ExecutionRef(), now) {
			continue
		}
		filtered = append(filtered, dt)
	}
	ready = filtered
	if len(ready) == 0 {
		return
	}
	if len(ready) > l.cfg.BatchSize {
		ready = ready[:l.cfg.BatchSize]
	}

	const maxConcurrent = 16
	concurrency := len(ready)
	if concurrency > maxConcurrent {
		concurrency = maxConcurrent
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	// A FANNED instance may only go to a peer that advertises instance identity.
	// An older peer ignores the task_run_id field it does not know and processes
	// the catalog id instead — which for an expanded group names N rows, so it
	// either strands a claim or drives the legacy group-wide write.  Resolved
	// lazily so an all-unfanned run pays nothing, and only once per tick.
	var capablePeers []peer
	capableResolved := false

	for _, dt := range ready {
		if ctx.Err() != nil {
			break
		}
		target := peers
		if dt.TaskRunID != uuid.Nil {
			if !capableResolved {
				capablePeers = l.instanceCapablePeers(ctx, peers)
				capableResolved = true
			}
			if len(capablePeers) == 0 {
				// Leave the instance on the ready queue: the next tick retries,
				// and a rolling deploy resolves this within one peer-cap TTL.
				log.Warn("dispatch loop: no peer advertises fan-out instance identity; deferring instance dispatch",
					"run_id", runID, "task_id", dt.TaskID, "task_run_id", dt.TaskRunID, "peers", len(peers))
				metrics.DispatchRejectedTotal.WithLabelValues(DispatchReasonNoCapablePeer).Inc()
				continue
			}
			target = capablePeers
		}
		idx := l.counter.Add(1) - 1
		p := target[idx%uint64(len(target))]
		// Carry both identities: the catalog task id (what every catalog lookup,
		// including the rate-limit rule, is keyed by) and the instance TaskRun id
		// (what the worker executes and fences its completion against).
		req := DispatchRequest{
			RunID:           runID,
			TaskID:          dt.TaskID,
			TaskRunID:       dt.TaskRunID,
			OwnerGeneration: generation,
			Attempt:         dt.Attempt,
			WorkerNode:      p.nodeID,
			OwnerBaseURL:    l.ownerBaseURL,
			Deadline:        time.Now().UTC().Add(l.cfg.Deadline),
		}
		execRef := dt.ExecutionRef()
		wg.Add(1)
		sem <- struct{}{}
		go func(p peer, req DispatchRequest, execRef uuid.UUID) {
			defer wg.Done()
			defer func() { <-sem }()
			if ok := l.acquireRateLimit(ctx, runID, req.TaskID, execRef); !ok {
				return
			}
			l.postOne(ctx, runID, p, req, false)
		}(p, req, execRef)
	}
	wg.Wait()
}

// reclaimExpiredClaims resets this run's rows whose worker claim lease lapsed
// back to pending, fenced on the owner generation, at most once per
// ownerReclaimInterval per run.  A store that does not implement the capability,
// or a run swept too recently, is a no-op.
func (l *DispatchLoop) reclaimExpiredClaims(runID uuid.UUID, generation int64) {
	reclaimer, ok := l.cfg.Store.(expiredClaimReclaimer)
	if !ok {
		return
	}
	if !l.dueForReclaim(runID, time.Now()) {
		return
	}
	rows, err := reclaimer.ReclaimOwnerExpiredClaims(runID, generation)
	if err != nil {
		log.Warn("dispatch loop: expired-claim reap failed", "run_id", runID, "error", err)
		return
	}
	if len(rows) > 0 {
		log.Warn("dispatch loop: re-queued tasks whose worker claim lease expired",
			"run_id", runID, "generation", generation, "count", len(rows))
	}
}

// dueForReclaim reports whether runID's expired-claim sweep is due, stamping the
// clock when it is.  A run seen for the first time is always due, so a lease
// that lapsed while this node was not the owner is recovered on the tick that
// adopts it rather than an interval later.
func (l *DispatchLoop) dueForReclaim(runID uuid.UUID, now time.Time) bool {
	l.reclaimMu.Lock()
	defer l.reclaimMu.Unlock()
	if last, seen := l.lastReclaim[runID]; seen && now.Sub(last) < ownerReclaimInterval {
		return false
	}
	l.lastReclaim[runID] = now
	return true
}

// forgetUnownedReclaims drops reclaim clocks for runs this node no longer owns,
// so the map tracks the owned set rather than growing for the life of the
// process.
func (l *DispatchLoop) forgetUnownedReclaims(owned map[uuid.UUID]int64) {
	l.reclaimMu.Lock()
	defer l.reclaimMu.Unlock()
	for runID := range l.lastReclaim {
		if _, still := owned[runID]; !still {
			delete(l.lastReclaim, runID)
		}
	}
}

// acquireRateLimit resolves and consumes the task's declared rate-limit budget.
//
// taskID must be the *catalog* task id: ratelimit.RuleForTask joins task_runs to
// tasks on task_id, so an instance id matches no row and the limit is silently
// skipped.  execRef is the row to park when the budget is exhausted — the
// instance for a fanned task, the catalog task otherwise — so one parked
// instance does not delay its siblings.
func (l *DispatchLoop) acquireRateLimit(ctx context.Context, runID, taskID, execRef uuid.UUID) bool {
	if l.cfg.RateLimiter == nil || l.cfg.RateLimitDB == nil {
		return true
	}
	if execRef == uuid.Nil {
		execRef = taskID
	}
	rule, ok, err := ratelimit.RuleForTask(ctx, l.cfg.RateLimitDB, runID, taskID)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("dispatch loop: rate limit lookup failed", "run_id", runID, "task_id", taskID, "error", err)
		}
		return false
	}
	if !ok {
		return true
	}
	acquired, err := l.cfg.RateLimiter.Acquire(ctx, rule.Resource, rule.Units, rule.Limit, rule.Window)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("dispatch loop: rate limit acquire failed", "run_id", runID, "task_id", taskID, "resource", rule.Resource, "error", err)
		}
		return false
	}
	if acquired {
		return true
	}

	updater, ok := l.cfg.Store.(rateLimitTaskUpdater)
	if !ok {
		log.Warn("dispatch loop: rate limit rejected task but store cannot requeue", "run_id", runID, "task_id", taskID, "resource", rule.Resource)
		return false
	}
	now := time.Now().UTC()
	retryAfter := now.Add(ratelimit.RetryAfter(now, rule.Window))
	if err := updater.RateLimitTask(ctx, runID, execRef, retryAfter); err != nil {
		if ctx.Err() == nil {
			log.Warn("dispatch loop: rate limit requeue failed", "run_id", runID, "task_id", taskID, "resource", rule.Resource, "error", err)
		}
		return false
	}
	if l.cfg.OwnerManager != nil {
		l.rememberRateLimitDelay(runID, execRef, retryAfter)
	}
	metrics.RunSkippedTotal.WithLabelValues(rule.JobAlias, "rate_limit").Inc()
	log.Info("dispatch loop: task delayed by rate limit", "run_id", runID, "task_id", taskID, "resource", rule.Resource, "retry_after", retryAfter)
	return false
}

func (l *DispatchLoop) rememberRateLimitDelay(runID, taskID uuid.UUID, retryAfter time.Time) {
	if retryAfter.IsZero() {
		return
	}
	l.rateLimitDelayMu.Lock()
	defer l.rateLimitDelayMu.Unlock()
	tasks := l.rateLimitDelays[runID]
	if tasks == nil {
		tasks = make(map[uuid.UUID]time.Time)
		l.rateLimitDelays[runID] = tasks
	}
	tasks[taskID] = retryAfter.UTC()
}

func (l *DispatchLoop) rateLimitDelayed(runID, taskID uuid.UUID, now time.Time) bool {
	l.rateLimitDelayMu.Lock()
	defer l.rateLimitDelayMu.Unlock()
	tasks := l.rateLimitDelays[runID]
	if tasks == nil {
		return false
	}
	retryAfter, ok := tasks[taskID]
	if !ok {
		return false
	}
	if retryAfter.After(now.UTC()) {
		return true
	}
	delete(tasks, taskID)
	if len(tasks) == 0 {
		delete(l.rateLimitDelays, runID)
	}
	return false
}

// postOne does the actual HTTP call + metric/log accounting for one dispatch.
func (l *DispatchLoop) postOne(ctx context.Context, runID uuid.UUID, p peer, req DispatchRequest, quarantined bool) {
	dispatchURL := p.baseURL + "/internal/dispatch"
	accepted, postErr := PostDispatch(ctx, dispatchURL, l.cfg.Token, req)
	if postErr != nil {
		if ctx.Err() != nil {
			return
		}
		// Network error = peer unreachable. Bench it so the next ticks skip it
		// rather than spending dispatchPostTimeout on it again.
		l.benchPeer(p.nodeID)
		log.Warn("dispatch loop: PostDispatch network error; benching peer",
			"run_id", runID,
			"task_id", req.TaskID,
			"peer", p.nodeID,
			"cooldown", peerBenchCooldown,
			"error", postErr,
		)
		if !quarantined {
			metrics.DispatchRejectedTotal.WithLabelValues(DispatchReasonNetworkError).Inc()
		}
		return
	}
	if !accepted {
		log.Warn("dispatch loop: worker rejected dispatch",
			"run_id", runID,
			"task_id", req.TaskID,
			"peer", p.nodeID,
		)
		if !quarantined {
			metrics.DispatchRejectedTotal.WithLabelValues(DispatchReasonWorkerRejected).Inc()
		}
		return
	}
	if !quarantined {
		metrics.DispatchSentTotal.Inc()
	}
	// In-memory mode: record the dispatch in the owner's RunState so the task
	// leaves the ready queue and becomes running (re-dispatched on lease expiry).
	if l.cfg.OwnerManager != nil {
		leaseMs := time.Now().Add(l.cfg.Deadline).UnixMilli()
		// RunState is keyed by instance identity for a fanned task; marking the
		// catalog task would leave the instance on the ready queue and
		// re-dispatch it every tick.
		dispatched := req.TaskRunID
		if dispatched == uuid.Nil {
			dispatched = req.TaskID
		}
		l.cfg.OwnerManager.MarkDispatched(runID, dispatched, p.nodeID, req.Attempt, leaseMs)
	}
	log.Debug("dispatch loop: task dispatched",
		"run_id", runID,
		"task_id", req.TaskID,
		"peer", p.nodeID,
	)
}

// instanceCapablePeers filters the rotation down to peers that advertise
// CapabilityInstanceIdentity, probing (and caching) any peer whose answer is
// missing or stale.
//
// Self is probed like any other peer, because "capable" means more than "runs a
// new enough build": a node with no worker attached advertises nothing and would
// reject the dispatch anyway.  What self gets is a different reading of a FAILED
// probe — see peerSupportsInstanceIdentity.
func (l *DispatchLoop) instanceCapablePeers(ctx context.Context, peers []peer) []peer {
	l.sweepPeerCaps(time.Now())
	out := make([]peer, 0, len(peers))
	for _, p := range peers {
		if l.peerSupportsInstanceIdentity(ctx, p) {
			out = append(out, p)
		}
	}
	return out
}

// peerSupportsInstanceIdentity answers from the cache when fresh, else probes
// the peer's /internal/capabilities and caches the answer.  Every failure —
// 404 from a build with no such route, an unreachable node, a malformed body —
// is cached as NOT capable: the gate fails closed, because guessing that a
// silent peer understands instance identity is exactly the bug it exists to
// prevent.
func (l *DispatchLoop) peerSupportsInstanceIdentity(ctx context.Context, p peer) bool {
	now := time.Now()
	l.capsMu.Lock()
	entry, cached := l.peerCaps[p.nodeID]
	l.capsMu.Unlock()
	if cached && now.Before(entry.expiresAt) {
		return entry.instanceIdentity
	}

	caps, err := GetCapabilities(ctx, p.baseURL+"/internal/capabilities", l.cfg.Token)
	if errors.Is(err, ErrPeerUnreachable) {
		// A peer we cannot reach at all costs a full probe timeout every negative
		// TTL otherwise.  Bench it on the same circuit breaker a failed dispatch
		// uses, so one dead node costs one timeout per cooldown rather than one
		// per tick.  A peer that ANSWERS 404 is not benched: it is a healthy older
		// node that must keep receiving unfanned work.
		l.benchPeer(p.nodeID)
	}
	supported := err == nil && caps.Supports(CapabilityInstanceIdentity)
	if err != nil && p.nodeID == l.cfg.NodeID {
		// A probe that could not reach OURSELVES says nothing about the
		// protocol — this node is running this build by definition.  Excluding
		// self on a transport blip would strand every fan-out on a single-node
		// install, the common case, for a mismatch that cannot exist there.  A
		// self-probe that ANSWERS is still taken at face value: a node with no
		// worker attached genuinely cannot execute an instance.
		supported = true
	}
	ttl := peerCapNegativeTTL
	if supported {
		ttl = peerCapPositiveTTL
	}
	l.capsMu.Lock()
	l.peerCaps[p.nodeID] = peerCapEntry{instanceIdentity: supported, expiresAt: now.Add(ttl)}
	l.capsMu.Unlock()

	if !supported && ctx.Err() == nil {
		log.Warn("dispatch loop: peer does not advertise fan-out instance identity; excluded from instance dispatch",
			"peer", p.nodeID, "error", err)
	}
	return supported
}

// sweepPeerCaps drops expired capability entries so a peer that left the cluster
// does not leak one forever (the per-peer path below only ever reaches peers
// still in the rotation).
func (l *DispatchLoop) sweepPeerCaps(now time.Time) {
	l.capsMu.Lock()
	defer l.capsMu.Unlock()
	for nodeID, entry := range l.peerCaps {
		if !now.Before(entry.expiresAt) {
			delete(l.peerCaps, nodeID)
		}
	}
}

// benchPeer marks a peer unreachable for peerBenchCooldown.  Self is never
// benched: the local node is presumed reachable, and benching it would only
// push work onto the all-benched fallback for no benefit.
func (l *DispatchLoop) benchPeer(nodeID string) {
	if nodeID == l.cfg.NodeID {
		return
	}
	l.benchMu.Lock()
	l.benchedPeers[nodeID] = time.Now().Add(peerBenchCooldown)
	l.benchMu.Unlock()
}

// healthyPeers returns peers not currently benched, dropping bench entries whose
// cooldown has lapsed so the peer is retried.  If filtering would leave no peers
// (every candidate benched), it returns the input unchanged so dispatch never
// starves on a transient cluster-wide blip.
func (l *DispatchLoop) healthyPeers(peers []peer) []peer {
	now := time.Now()
	l.benchMu.Lock()
	defer l.benchMu.Unlock()

	// Drop every expired bench entry (not just ones in `peers`), so a peer that
	// was benched and then permanently removed from cluster membership doesn't
	// leak its entry forever.  Sweeping the whole map here is what bounds it: a
	// peer that left the cluster stops appearing in `peers`, so the per-peer loop
	// below would never reach its (now-expired) entry to delete it.
	for nodeID, until := range l.benchedPeers {
		if !now.Before(until) {
			delete(l.benchedPeers, nodeID)
		}
	}

	out := make([]peer, 0, len(peers))
	for _, p := range peers {
		if _, benched := l.benchedPeers[p.nodeID]; benched {
			continue
		}
		out = append(out, p)
	}
	// Fallback: only triggers when healthyPeers is called with a peer list that
	// excludes self (e.g. in unit tests).  Via tick(), buildPeers always appends
	// self and benchPeer never benches self, so out is never empty in production
	// and this branch does not fire there.  Kept as harmless defense-in-depth.
	if len(out) == 0 {
		return peers
	}
	return out
}

// buildPeers normalises raw peer addresses (host:dqlitePort) into peer pairs
// of {nodeID = host:dqlitePort, baseURL = http://host:apiPort}.  Self is
// always appended at the end so the round-robin always has at least one target.
func (l *DispatchLoop) buildPeers(rawPeers []string) []peer {
	seen := make(map[string]struct{}, len(rawPeers)+1)
	out := make([]peer, 0, len(rawPeers)+1)

	add := func(addr string) {
		canonical := strings.TrimSpace(addr)
		if canonical == "" || strings.HasPrefix(canonical, "@") {
			return
		}
		if _, ok := seen[canonical]; ok {
			return
		}
		baseURL := l.nodeAddrToBaseURL(canonical)
		if baseURL == "" {
			return
		}
		seen[canonical] = struct{}{}
		out = append(out, peer{nodeID: canonical, baseURL: baseURL})
	}

	for _, addr := range rawPeers {
		add(addr)
	}
	// Always include self so single-node setups dispatch to themselves.
	add(l.cfg.NodeID)

	return out
}

// nodeAddrToBaseURL converts "host:dqlitePort" (the dqlite / CAESIUM_NODE_ADDRESS
// format) to "http://host:apiPort".  Returns "" on parse failure. The config's
// PeerBaseURL override takes precedence when set.
func (l *DispatchLoop) nodeAddrToBaseURL(nodeAddr string) string {
	if l.cfg.PeerBaseURL != nil {
		return l.cfg.PeerBaseURL(nodeAddr)
	}
	host, _, err := net.SplitHostPort(nodeAddr)
	if err != nil {
		host = strings.TrimSpace(nodeAddr)
	}
	if host == "" || strings.HasPrefix(host, "@") {
		return ""
	}
	scheme, port := "http", l.cfg.APIPort
	if l.cfg.InternalPort > 0 {
		scheme, port = "https", l.cfg.InternalPort
	}
	return (&url.URL{
		Scheme: scheme,
		Host:   net.JoinHostPort(host, strconv.Itoa(port)),
	}).String()
}
