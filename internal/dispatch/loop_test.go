package dispatch

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

// -- Test helpers ------------------------------------------------------------

// testPeerLister is a stub PeerLister that returns a fixed set of addresses.
type testPeerLister struct {
	mu    sync.Mutex
	peers []string
}

func (p *testPeerLister) DispatchPeers(_ context.Context) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.peers...), nil
}

// testOwnerReader wraps run.LeaseStore to satisfy OwnerReader.
type testOwnerReader struct {
	ls *run.LeaseStore
}

func (r *testOwnerReader) OwnedRunsWithGenerations(ctx context.Context, ownerNode string) (map[uuid.UUID]int64, error) {
	return r.ls.OwnedRunsWithGenerations(ctx, ownerNode)
}

// testTaskReader wraps run.Store to satisfy TaskPendingReader.
type testTaskReader struct {
	s *run.Store
}

func (r *testTaskReader) PendingTasksForDispatch(ctx context.Context, runID uuid.UUID, limit int) ([]models.TaskRun, error) {
	return r.s.PendingTasksForDispatch(ctx, runID, limit)
}

// counterValue reads the current value of a prometheus.Counter.
func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("counter.Write: %v", err)
	}
	return m.GetCounter().GetValue()
}

// counterVecValue reads the current value of a prometheus.CounterVec for the
// given label values.
func counterVecValue(t *testing.T, cv *prometheus.CounterVec, lvs ...string) float64 {
	t.Helper()
	c, err := cv.GetMetricWithLabelValues(lvs...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues(%v): %v", lvs, err)
	}
	return counterValue(t, c)
}

// serverPort parses the port from an httptest.Server's listener address string
// ("127.0.0.1:PORT") and returns it as an int.
func serverPort(t *testing.T, s *httptest.Server) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(s.Listener.Addr().String())
	require.NoError(t, err, "parse server listener port")
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err, "convert port to int")
	return port
}

// serverNodeID returns a "127.0.0.1:PORT" style node address that the dispatch
// loop will convert to http://127.0.0.1:PORT using the same port (apiPort ==
// serverPort(s)).  This lets us route loop dispatches to the test server.
func serverNodeID(t *testing.T, s *httptest.Server) (nodeID string, apiPort int) {
	t.Helper()
	apiPort = serverPort(t, s)
	nodeID = net.JoinHostPort("127.0.0.1", strconv.Itoa(apiPort))
	return nodeID, apiPort
}

// -- Constants ---------------------------------------------------------------

const (
	loopToken = "loop-test-token"
)

// -- Test DB helpers ---------------------------------------------------------

// insertPendingTask inserts a minimal pending task_runs row directly using the
// underlying gorm connection exposed via store.DB().
func insertPendingTask(t *testing.T, store *run.Store, runID, taskID uuid.UUID) {
	t.Helper()

	task := &models.TaskRun{
		ID:                      uuid.New(),
		JobRunID:                runID,
		TaskID:                  taskID,
		AtomID:                  uuid.New(),
		Engine:                  "docker",
		Image:                   "busybox:1.36.1", // pinned to pass guardrails check
		Command:                 "[]",
		Status:                  string(run.TaskStatusPending),
		ClaimedBy:               "",
		OutstandingPredecessors: 0,
		Attempt:                 1,
		MaxAttempts:             1,
	}
	require.NoError(t, store.DB().Create(task).Error)
}

// -- Tests -------------------------------------------------------------------

// TestDispatchLoop_HappyPath verifies that the loop finds one ready task on an
// owned run, dispatches it to a stub worker that returns 202, and increments
// caesium_dispatch_sent_total.
func TestDispatchLoop_HappyPath(t *testing.T) {
	metrics.Register()

	// Track how many dispatch calls arrive at the stub server.
	var received atomic.Int32

	// Stub worker: accepts dispatches unconditionally.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		var req DispatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		require.Equal(t, 1, req.Attempt)
		require.NotZero(t, req.OwnerGeneration)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	nodeID, apiPort := serverNodeID(t, server)

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)

	runID := uuid.New()
	_, err := ls.AcquireLease(context.Background(), runID, nodeID, 30*time.Second)
	require.NoError(t, err)
	taskID := uuid.New()
	insertPendingTask(t, store, runID, taskID)

	beforeSent := counterValue(t, metrics.DispatchSentTotal)

	loop := NewDispatchLoop(DispatchLoopConfig{
		NodeID:     nodeID,
		APIPort:    apiPort,
		Token:      loopToken,
		Interval:   50 * time.Millisecond,
		BatchSize:  64,
		Deadline:   5 * time.Minute,
		LeaseStore: &testOwnerReader{ls},
		Store:      &testTaskReader{store},
		Peers:      &testPeerLister{}, // no external peers; self is added automatically
	})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	t.Cleanup(cancel)
	loop.Run(ctx)

	require.GreaterOrEqual(t, received.Load(), int32(1), "should have dispatched at least once")
	afterSent := counterValue(t, metrics.DispatchSentTotal)
	require.Greater(t, afterSent, beforeSent, "caesium_dispatch_sent_total should have incremented")
}

// TestDispatchLoop_FallbackOnRejection verifies that when the stub worker
// returns 409, the task remains pending+unclaimed, the rejection counter
// increments, and the next tick dispatches again.
func TestDispatchLoop_FallbackOnRejection(t *testing.T) {
	metrics.Register()

	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusConflict) // 409 — reject
	}))
	t.Cleanup(server.Close)

	nodeID, apiPort := serverNodeID(t, server)

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)

	runID := uuid.New()
	_, err := ls.AcquireLease(context.Background(), runID, nodeID, 30*time.Second)
	require.NoError(t, err)
	taskID := uuid.New()
	insertPendingTask(t, store, runID, taskID)

	beforeRejected := counterVecValue(t, metrics.DispatchRejectedTotal, DispatchReasonWorkerRejected)

	loop := NewDispatchLoop(DispatchLoopConfig{
		NodeID:     nodeID,
		APIPort:    apiPort,
		Token:      loopToken,
		Interval:   50 * time.Millisecond,
		BatchSize:  64,
		Deadline:   5 * time.Minute,
		LeaseStore: &testOwnerReader{ls},
		Store:      &testTaskReader{store},
		Peers:      &testPeerLister{},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	t.Cleanup(cancel)
	loop.Run(ctx)

	afterRejected := counterVecValue(t, metrics.DispatchRejectedTotal, DispatchReasonWorkerRejected)
	require.Greater(t, afterRejected, beforeRejected,
		"caesium_dispatch_rejected_total{worker_rejected} should increment")

	// Task must still be pending+unclaimed since dispatch was always rejected.
	tasks, err := store.PendingTasksForDispatch(context.Background(), runID, 10)
	require.NoError(t, err)
	require.Len(t, tasks, 1, "task should remain pending+unclaimed after rejected dispatch")

	require.GreaterOrEqual(t, received.Load(), int32(1), "stub should have received at least one request")
}

// TestDispatchLoop_NoOwnedRuns verifies that when the node owns no runs, the
// loop exits early without calling PostDispatch.
func TestDispatchLoop_NoOwnedRuns(t *testing.T) {
	metrics.Register()

	var called atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	nodeID, apiPort := serverNodeID(t, server)

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)
	// No leases acquired — OwnedRuns returns empty.

	loop := NewDispatchLoop(DispatchLoopConfig{
		NodeID:     nodeID,
		APIPort:    apiPort,
		Token:      loopToken,
		Interval:   50 * time.Millisecond,
		BatchSize:  64,
		Deadline:   5 * time.Minute,
		LeaseStore: &testOwnerReader{ls},
		Store:      &testTaskReader{store},
		Peers:      &testPeerLister{},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	t.Cleanup(cancel)
	loop.Run(ctx)

	require.False(t, called.Load(), "PostDispatch should never be called when no runs are owned")
}

// TestDispatchLoop_NoPeers verifies that when peer discovery returns empty AND
// the node ID is non-routable, the loop increments
// caesium_dispatch_rejected_total{reason=no_peers} and exits the tick early.
func TestDispatchLoop_NoPeers(t *testing.T) {
	metrics.Register()

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)

	// Acquire a lease so OwnedRuns returns a non-empty set.
	const nodeID = "@abstract-unix" // abstract-unix address; skipped by buildPeerURLs
	runID := uuid.New()
	_, err := ls.AcquireLease(context.Background(), runID, nodeID, 30*time.Second)
	require.NoError(t, err)

	// Peer lister returns empty; NodeID is abstract so self-URL is also empty.
	peers := &testPeerLister{}

	beforeNoPeers := counterVecValue(t, metrics.DispatchRejectedTotal, DispatchReasonNoPeers)

	loop := NewDispatchLoop(DispatchLoopConfig{
		NodeID:     nodeID, // abstract unix — skipped by buildPeerURLs; no self URL
		APIPort:    8080,
		Token:      loopToken,
		Interval:   50 * time.Millisecond,
		BatchSize:  64,
		Deadline:   5 * time.Minute,
		LeaseStore: &testOwnerReader{ls},
		Store:      &testTaskReader{store},
		Peers:      peers,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	t.Cleanup(cancel)
	loop.Run(ctx)

	afterNoPeers := counterVecValue(t, metrics.DispatchRejectedTotal, DispatchReasonNoPeers)
	require.Greater(t, afterNoPeers, beforeNoPeers,
		"caesium_dispatch_rejected_total{no_peers} should increment when no peer URLs resolve")
}

// TestDispatchLoop_BatchCap verifies that when an owned run has more ready
// tasks than the batch limit, only BatchSize tasks are returned per
// PendingTasksForDispatch call.
func TestDispatchLoop_BatchCap(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)

	runID := uuid.New()
	const nodeID = "127.0.0.1:9001"
	_, err := ls.AcquireLease(context.Background(), runID, nodeID, 30*time.Second)
	require.NoError(t, err)

	const totalTasks = 200
	const batchSize = 64

	for i := 0; i < totalTasks; i++ {
		insertPendingTask(t, store, runID, uuid.New())
	}

	// PendingTasksForDispatch with limit=batchSize should cap the result.
	tasks, err := store.PendingTasksForDispatch(context.Background(), runID, batchSize)
	require.NoError(t, err)
	require.Len(t, tasks, batchSize,
		"PendingTasksForDispatch with limit=%d should return at most %d tasks from %d",
		batchSize, batchSize, totalTasks)

	// Verify that with a larger limit we get all tasks.
	allTasks, err := store.PendingTasksForDispatch(context.Background(), runID, totalTasks+1)
	require.NoError(t, err)
	require.Len(t, allTasks, totalTasks,
		"PendingTasksForDispatch with limit>total should return all %d tasks", totalTasks)
}

// TestDispatchLoop_SelfDispatch verifies that when there is exactly one peer
// (self), the dispatch envelope's WorkerNode is populated and dispatches reach
// the local node.
func TestDispatchLoop_SelfDispatch(t *testing.T) {
	metrics.Register()

	var capturedReq DispatchRequest
	var mu sync.Mutex
	var received atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		var req DispatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			mu.Lock()
			capturedReq = req
			mu.Unlock()
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	nodeID, apiPort := serverNodeID(t, server)

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)
	runID := uuid.New()
	_, err := ls.AcquireLease(context.Background(), runID, nodeID, 30*time.Second)
	require.NoError(t, err)
	insertPendingTask(t, store, runID, uuid.New())

	loop := NewDispatchLoop(DispatchLoopConfig{
		NodeID:     nodeID, // single peer: self
		APIPort:    apiPort,
		Token:      loopToken,
		Interval:   50 * time.Millisecond,
		BatchSize:  64,
		Deadline:   5 * time.Minute,
		LeaseStore: &testOwnerReader{ls},
		Store:      &testTaskReader{store},
		Peers:      &testPeerLister{}, // no external peers
	})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	t.Cleanup(cancel)
	loop.Run(ctx)

	require.GreaterOrEqual(t, received.Load(), int32(1), "should have dispatched to self at least once")

	mu.Lock()
	wn := capturedReq.WorkerNode
	mu.Unlock()

	require.NotEmpty(t, wn, "WorkerNode should be set in the DispatchRequest")
	require.Contains(t, wn, "127.0.0.1",
		"WorkerNode should reference the local host")
}

// TestDispatchLoop_RoundRobin verifies that 6 tasks across 3 peer node IDs
// are distributed exactly 2 per peer. All dispatches reach a single mux server
// (so the test doesn't have to engineer 3 servers on the same port); the
// distribution is observed via the WorkerNode field on each received
// DispatchRequest. This is enabled by the PeerBaseURL config override, which
// the test points at the mux for every peer node ID.
func TestDispatchLoop_RoundRobin(t *testing.T) {
	metrics.Register()

	const numTasks = 6

	// Single mux server captures every dispatch and records its WorkerNode.
	var (
		wnMu        sync.Mutex
		workerNodes []string
	)
	mux := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req DispatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			wnMu.Lock()
			workerNodes = append(workerNodes, req.WorkerNode)
			wnMu.Unlock()
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(mux.Close)

	// 3 logical peer node IDs (matching the dqlite-port style that
	// CAESIUM_NODE_ADDRESS uses). Self is omitted so buildPeers gives us
	// exactly these three plus self (the owner) for 4 total — but the owner's
	// nodeID is one of these three, so dedup keeps it at 3.
	const ownerNodeID = "node-a:9001"
	peerNodeIDs := []string{ownerNodeID, "node-b:9001", "node-c:9001"}

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)

	runID := uuid.New()
	_, err := ls.AcquireLease(context.Background(), runID, ownerNodeID, 30*time.Second)
	require.NoError(t, err)
	for i := 0; i < numTasks; i++ {
		insertPendingTask(t, store, runID, uuid.New())
	}

	loop := NewDispatchLoop(DispatchLoopConfig{
		NodeID:     ownerNodeID,
		APIPort:    8080, // unused because PeerBaseURL overrides
		Token:      loopToken,
		Interval:   time.Hour, // we call tick() directly
		BatchSize:  64,
		Deadline:   5 * time.Minute,
		LeaseStore: &testOwnerReader{ls},
		Store:      &testTaskReader{store},
		Peers:      &testPeerLister{peers: peerNodeIDs},
		// All peer node IDs resolve to the same mux URL so dispatches actually
		// land somewhere observable; WorkerNode keeps the original nodeID so we
		// can verify distribution.
		PeerBaseURL: func(_ string) string { return mux.URL },
	})

	loop.tick(context.Background())

	wnMu.Lock()
	got := append([]string(nil), workerNodes...)
	wnMu.Unlock()

	require.Len(t, got, numTasks, "every task should be dispatched in one tick")

	counts := map[string]int{}
	for _, wn := range got {
		counts[wn]++
	}
	for _, peerID := range peerNodeIDs {
		require.Equal(t, numTasks/len(peerNodeIDs), counts[peerID],
			"peer %q should receive numTasks/numPeers=%d dispatches, got %d",
			peerID, numTasks/len(peerNodeIDs), counts[peerID])
	}
}
