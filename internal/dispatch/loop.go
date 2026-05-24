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
	// URL from peer node addresses.
	APIPort int
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
}

// DispatchLoop is the per-node push-dispatch goroutine for Phase A2.
// Call Run(ctx) in a goroutine; it exits cleanly when ctx is cancelled.
type DispatchLoop struct {
	cfg     DispatchLoopConfig
	counter atomic.Uint64 // round-robin counter; used modulo peer count
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
	return &DispatchLoop{cfg: cfg}
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
			Deadline:   time.Now().UTC().Add(l.cfg.Deadline),
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
	return (&url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(host, strconv.Itoa(l.cfg.APIPort)),
	}).String()
}
