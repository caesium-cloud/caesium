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
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
)

// DispatchRejectionReason labels for caesium_dispatch_rejected_total.
const (
	DispatchReasonNetworkError   = "network_error"
	DispatchReasonWorkerRejected = "worker_rejected"
	DispatchReasonNoPeers        = "no_peers"
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
type OwnerReader interface {
	OwnedRuns(ctx context.Context, ownerNode string) ([]uuid.UUID, error)
	GetLease(ctx context.Context, runID uuid.UUID) (*models.RunLease, error)
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
	peers, err := l.cfg.Peers.DispatchPeers(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Warn("dispatch loop: peer discovery failed", "error", err)
		metrics.DispatchRejectedTotal.WithLabelValues(DispatchReasonNoPeers).Inc()
		return
	}
	// Normalise peers to dispatch URLs; include self in the rotation.
	peerURLs := l.buildPeerURLs(peers)
	if len(peerURLs) == 0 {
		metrics.DispatchRejectedTotal.WithLabelValues(DispatchReasonNoPeers).Inc()
		return
	}

	// 2. Find runs this node owns.
	ownedRunIDs, err := l.cfg.LeaseStore.OwnedRuns(ctx, l.cfg.NodeID)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Warn("dispatch loop: OwnedRuns failed", "error", err)
		return
	}
	if len(ownedRunIDs) == 0 {
		return // nothing to do
	}

	// 3. For each owned run, find ready tasks and dispatch them.
	for _, runID := range ownedRunIDs {
		if ctx.Err() != nil {
			return
		}
		l.dispatchRun(ctx, runID, peerURLs)
	}
}

// dispatchRun dispatches up to BatchSize ready tasks for a single owned run.
func (l *DispatchLoop) dispatchRun(ctx context.Context, runID uuid.UUID, peerURLs []string) {
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

	// Get the current lease generation for this run so we can stamp it into
	// the DispatchRequest.  A GetLease call per owned run is acceptable: we
	// only poll runs we own, and the count is bounded by CAESIUM_RUN_OWNER_MAX_RUNS.
	lease, err := l.cfg.LeaseStore.GetLease(ctx, runID)
	if err != nil || lease == nil {
		if ctx.Err() != nil {
			return
		}
		log.Warn("dispatch loop: GetLease failed; skipping run",
			"run_id", runID, "error", err)
		return
	}

	for i := range tasks {
		if ctx.Err() != nil {
			return
		}
		task := &tasks[i]

		// Pick a peer via round-robin (atomic counter so concurrent ticks
		// from different runs don't drift, even though Phase A2 only runs a
		// single tick goroutine).
		idx := l.counter.Add(1) - 1
		peerURL := peerURLs[idx%uint64(len(peerURLs))]

		req := DispatchRequest{
			RunID:           runID,
			TaskID:          task.TaskID,
			OwnerGeneration: lease.Generation,
			Attempt:         task.Attempt,
			WorkerNode:      peerNodeIDFromURL(peerURL, l.cfg.APIPort),
			Deadline:        time.Now().UTC().Add(l.cfg.Deadline),
		}

		dispatchURL := peerURL + "/internal/dispatch"
		accepted, postErr := PostDispatch(ctx, dispatchURL, l.cfg.Token, req)
		if postErr != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warn("dispatch loop: PostDispatch network error",
				"run_id", runID,
				"task_id", task.TaskID,
				"peer", peerURL,
				"error", postErr,
			)
			metrics.DispatchRejectedTotal.WithLabelValues(DispatchReasonNetworkError).Inc()
			// Leave task unclaimed; ClaimNext recovery will pick it up.
			continue
		}
		if !accepted {
			log.Warn("dispatch loop: worker rejected dispatch",
				"run_id", runID,
				"task_id", task.TaskID,
				"peer", peerURL,
			)
			metrics.DispatchRejectedTotal.WithLabelValues(DispatchReasonWorkerRejected).Inc()
			// Leave task unclaimed; ClaimNext recovery will pick it up.
			continue
		}

		metrics.DispatchSentTotal.Inc()
		log.Debug("dispatch loop: task dispatched",
			"run_id", runID,
			"task_id", task.TaskID,
			"peer", peerURL,
		)
	}
}

// buildPeerURLs converts raw node addresses (host:dqlitePort) to base HTTP
// URLs (http://host:apiPort) suitable for PostDispatch.  Self is always
// appended at the end so the round-robin always has at least one target.
func (l *DispatchLoop) buildPeerURLs(peers []string) []string {
	seen := make(map[string]struct{}, len(peers)+1)
	urls := make([]string, 0, len(peers)+1)

	for _, addr := range peers {
		u := l.nodeAddrToBaseURL(addr)
		if u == "" {
			continue
		}
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		urls = append(urls, u)
	}

	// Always include self so single-node setups dispatch to themselves.
	selfURL := l.nodeAddrToBaseURL(l.cfg.NodeID)
	if selfURL != "" {
		if _, ok := seen[selfURL]; !ok {
			seen[selfURL] = struct{}{}
			urls = append(urls, selfURL)
		}
	}

	return urls
}

// nodeAddrToBaseURL converts "host:dqlitePort" (the dqlite / CAESIUM_NODE_ADDRESS
// format) to "http://host:apiPort".  Returns "" on parse failure.
func (l *DispatchLoop) nodeAddrToBaseURL(nodeAddr string) string {
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

// peerNodeIDFromURL extracts the "host:apiPort" node identity from the base
// URL that was constructed by buildPeerURLs / nodeAddrToBaseURL.  The returned
// string is used as WorkerNode in the DispatchRequest so the recipient can
// validate "dispatch addressed to me".
func peerNodeIDFromURL(baseURL string, apiPort int) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return baseURL
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		// No port in URL host — shouldn't happen given buildPeerURLs, but be safe.
		return fmt.Sprintf("%s:%d", u.Host, apiPort)
	}
	_ = port
	return net.JoinHostPort(host, strconv.Itoa(apiPort))
}
