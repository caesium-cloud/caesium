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
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
)

// DispatchRejectionReason labels for caesium_dispatch_rejected_total.
const (
	DispatchReasonNetworkError       = "network_error"
	DispatchReasonWorkerRejected     = "worker_rejected"
	DispatchReasonNoPeers            = "no_peers"             // peer discovery returned empty list (bootstrap)
	DispatchReasonPeerDiscoveryError = "peer_discovery_error" // peer discovery RPC failed
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
}

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
	l := &DispatchLoop{cfg: cfg}
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
	// 1. Discover peers (includes self).
	rawPeers, err := l.cfg.Peers.DispatchPeers(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Warn("dispatch loop: peer discovery failed", "error", err)
		// Distinct from no_peers (empty list = normal bootstrap) so dashboards
		// can alert on real RPC failures separately.
		metrics.DispatchRejectedTotal.WithLabelValues(DispatchReasonPeerDiscoveryError).Inc()
		return
	}
	// Normalise peers to {nodeID, baseURL} pairs; include self in the rotation.
	peers := l.buildPeers(rawPeers)
	if len(peers) == 0 {
		metrics.DispatchRejectedTotal.WithLabelValues(DispatchReasonNoPeers).Inc()
		return
	}

	// 1b. Failover sweep (in-memory mode only): take over any lease whose owner
	//     let it expire, so a dead owner's runs get a live owner that recovers
	//     and resumes them.  In SQL mode, ClaimNext recovery handles this instead.
	if l.cfg.OwnerManager != nil {
		if n, takeErr := l.cfg.LeaseStore.AcquireExpiredLeases(ctx, l.cfg.NodeID, l.cfg.LeaseTTL); takeErr != nil {
			if ctx.Err() == nil {
				log.Warn("dispatch loop: expired-lease takeover failed", "error", takeErr)
			}
		} else if n > 0 {
			log.Info("dispatch loop: took over expired run leases", "count", n, "new_owner", l.cfg.NodeID)
		}
	}

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
			RunID:           runID,
			TaskID:          task.TaskID,
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
			l.postOne(ctx, runID, p, req)
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

	ready := mgr.ReadyForDispatch(runID)
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

	for _, dt := range ready {
		if ctx.Err() != nil {
			break
		}
		idx := l.counter.Add(1) - 1
		p := peers[idx%uint64(len(peers))]
		req := DispatchRequest{
			RunID:           runID,
			TaskID:          dt.TaskID,
			OwnerGeneration: generation,
			Attempt:         dt.Attempt,
			WorkerNode:      p.nodeID,
			OwnerBaseURL:    l.ownerBaseURL,
			Deadline:        time.Now().UTC().Add(l.cfg.Deadline),
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(p peer, req DispatchRequest) {
			defer wg.Done()
			defer func() { <-sem }()
			l.postOne(ctx, runID, p, req)
		}(p, req)
	}
	wg.Wait()
}

// postOne does the actual HTTP call + metric/log accounting for one dispatch.
func (l *DispatchLoop) postOne(ctx context.Context, runID uuid.UUID, p peer, req DispatchRequest) {
	dispatchURL := p.baseURL + "/internal/dispatch"
	accepted, postErr := PostDispatch(ctx, dispatchURL, l.cfg.Token, req)
	if postErr != nil {
		if ctx.Err() != nil {
			return
		}
		log.Warn("dispatch loop: PostDispatch network error",
			"run_id", runID,
			"task_id", req.TaskID,
			"peer", p.nodeID,
			"error", postErr,
		)
		metrics.DispatchRejectedTotal.WithLabelValues(DispatchReasonNetworkError).Inc()
		return
	}
	if !accepted {
		log.Warn("dispatch loop: worker rejected dispatch",
			"run_id", runID,
			"task_id", req.TaskID,
			"peer", p.nodeID,
		)
		metrics.DispatchRejectedTotal.WithLabelValues(DispatchReasonWorkerRejected).Inc()
		return
	}
	metrics.DispatchSentTotal.Inc()
	// In-memory mode: record the dispatch in the owner's RunState so the task
	// leaves the ready queue and becomes running (re-dispatched on lease expiry).
	if l.cfg.OwnerManager != nil {
		leaseMs := time.Now().Add(l.cfg.Deadline).UnixMilli()
		l.cfg.OwnerManager.MarkDispatched(runID, req.TaskID, p.nodeID, req.Attempt, leaseMs)
	}
	log.Debug("dispatch loop: task dispatched",
		"run_id", runID,
		"task_id", req.TaskID,
		"peer", p.nodeID,
	)
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
