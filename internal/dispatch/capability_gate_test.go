package dispatch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// peerStub is a fake node that records what the dispatch loop asked of it and
// answers the capability probe however the test wants.
type peerStub struct {
	server *httptest.Server

	mu         sync.Mutex
	dispatched []DispatchRequest
	probes     int
}

// newPeerStub builds a stub whose /internal/capabilities answers with caps, or
// 404s when caps is nil — which is exactly how a build from before
// instance-addressed dispatch behaves.
func newPeerStub(t *testing.T, caps *CapabilitiesResponse) *peerStub {
	t.Helper()
	p := &peerStub{}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/capabilities":
			p.mu.Lock()
			p.probes++
			p.mu.Unlock()
			if caps == nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(caps)
		case "/internal/dispatch":
			var req DispatchRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			p.mu.Lock()
			p.dispatched = append(p.dispatched, req)
			p.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *peerStub) requests() []DispatchRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]DispatchRequest(nil), p.dispatched...)
}

func (p *peerStub) probeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.probes
}

func capableResponse(nodeID string) *CapabilitiesResponse {
	return &CapabilitiesResponse{
		NodeID:          nodeID,
		ProtocolVersion: InternalProtocolVersion,
		Capabilities:    []string{CapabilityInstanceIdentity},
	}
}

// TestDispatchRunInMemory_FannedInstancesOnlyGoToCapablePeers pins the
// rolling-deploy gate.
//
// A peer running a build from before instance-addressed dispatch ignores the
// task_run_id field it does not know and processes the CATALOG id instead —
// which, for an expanded fan-out group, resolves to N rows.  Sending an instance
// there either strands the claim on an ambiguity error or drives a legacy
// group-wide write.  The owner therefore asks each peer what it supports and
// routes instances only to peers that say so.
func TestDispatchRunInMemory_FannedInstancesOnlyGoToCapablePeers(t *testing.T) {
	const (
		modernNodeID = "10.0.0.1:9001"
		legacyNodeID = "10.0.0.2:9001"
	)
	modern := newPeerStub(t, capableResponse(modernNodeID))
	legacy := newPeerStub(t, nil) // pre-capability build: 404s the probe

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)

	runID, taskID, instanceIDs := seedFannedRun(t, db, store)
	_, err := ls.AcquireLease(context.Background(), runID, modernNodeID, 30*time.Second)
	require.NoError(t, err)

	mgr := run.NewOwnerManager(store, run.CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	loop := NewDispatchLoop(DispatchLoopConfig{
		NodeID:     modernNodeID,
		Token:      loopToken,
		Interval:   40 * time.Millisecond,
		BatchSize:  64,
		Deadline:   5 * time.Minute,
		LeaseTTL:   30 * time.Second,
		LeaseStore: &testOwnerReader{ls},
		Store:      &testTaskReader{store},
		Peers:      &testPeerLister{peers: []string{legacyNodeID}},
		PeerBaseURL: func(addr string) string {
			switch addr {
			case modernNodeID:
				return modern.server.URL
			case legacyNodeID:
				return legacy.server.URL
			}
			return ""
		},
		OwnerManager: mgr,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	t.Cleanup(cancel)
	loop.Run(ctx)

	require.Empty(t, legacy.requests(),
		"a peer that does not advertise instance identity must never receive a fan-out instance")
	require.GreaterOrEqual(t, legacy.probeCount(), 1,
		"the owner must actually ask the peer before excluding it")

	got := modern.requests()
	require.Len(t, got, 3, "every instance must still be dispatched, to the capable peer")
	seen := map[uuid.UUID]bool{}
	for _, req := range got {
		require.Equal(t, taskID, req.TaskID)
		require.NotEqual(t, uuid.Nil, req.TaskRunID)
		seen[req.TaskRunID] = true
	}
	for _, id := range instanceIDs {
		require.True(t, seen[id], "instance %s was never dispatched", id)
	}
}

// TestDispatchRunInMemory_NoCapablePeerDefersTheInstance covers the window where
// EVERY live peer predates instance identity — including this node, which has no
// worker attached and so advertises nothing.  There is nowhere safe to send the
// instance, so it stays on the ready queue and the stall is counted rather than
// papered over by dispatching it somewhere that would mishandle it.
func TestDispatchRunInMemory_NoCapablePeerDefersTheInstance(t *testing.T) {
	const (
		selfNodeID   = "10.0.0.1:9001"
		legacyNodeID = "10.0.0.2:9001"
	)
	// This node answers the probe honestly: no worker, so no capabilities.
	self := newPeerStub(t, &CapabilitiesResponse{
		NodeID:          selfNodeID,
		ProtocolVersion: InternalProtocolVersion,
		Capabilities:    []string{},
	})
	legacy := newPeerStub(t, nil)

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)

	runID, _, _ := seedFannedRun(t, db, store)
	_, err := ls.AcquireLease(context.Background(), runID, selfNodeID, 30*time.Second)
	require.NoError(t, err)

	before := counterVecValue(t, metrics.DispatchRejectedTotal, DispatchReasonNoCapablePeer)

	mgr := run.NewOwnerManager(store, run.CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	loop := NewDispatchLoop(DispatchLoopConfig{
		NodeID:     selfNodeID,
		Token:      loopToken,
		Interval:   40 * time.Millisecond,
		BatchSize:  64,
		Deadline:   5 * time.Minute,
		LeaseTTL:   30 * time.Second,
		LeaseStore: &testOwnerReader{ls},
		Store:      &testTaskReader{store},
		Peers:      &testPeerLister{peers: []string{legacyNodeID}},
		PeerBaseURL: func(addr string) string {
			switch addr {
			case selfNodeID:
				return self.server.URL
			case legacyNodeID:
				return legacy.server.URL
			}
			return ""
		},
		OwnerManager: mgr,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	t.Cleanup(cancel)
	loop.Run(ctx)

	require.Empty(t, self.requests(), "an instance must not be sent to a node that advertises nothing")
	require.Empty(t, legacy.requests(), "an instance must not be sent to a pre-capability node")
	require.Greater(t, counterVecValue(t, metrics.DispatchRejectedTotal, DispatchReasonNoCapablePeer), before,
		"a deferred instance must be visible as a stall, not silently dropped")

	// The work is deferred, not lost: it is still dispatchable.
	require.NotEmpty(t, mgr.ReadyForDispatch(runID),
		"a deferred instance must stay on the ready queue for the next tick")
}

// TestDispatchRunInMemory_UnfannedWorkIgnoresTheCapabilityGate is the control on
// blast radius: an unfanned task carries no TaskRunID, is unambiguous by its
// catalog id on any build, and must dispatch exactly as before — without the
// owner paying for a capability probe at all.
func TestDispatchRunInMemory_UnfannedWorkIgnoresTheCapabilityGate(t *testing.T) {
	const legacyNodeID = "10.0.0.2:9001"
	legacy := newPeerStub(t, nil)

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)

	runID, taskID := seedPendingTaskRun(t, store)
	_, err := ls.AcquireLease(context.Background(), runID, legacyNodeID, 30*time.Second)
	require.NoError(t, err)

	mgr := run.NewOwnerManager(store, run.CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	loop := NewDispatchLoop(DispatchLoopConfig{
		NodeID:       legacyNodeID,
		Token:        loopToken,
		Interval:     40 * time.Millisecond,
		BatchSize:    64,
		Deadline:     5 * time.Minute,
		LeaseTTL:     30 * time.Second,
		LeaseStore:   &testOwnerReader{ls},
		Store:        &testTaskReader{store},
		Peers:        &testPeerLister{},
		PeerBaseURL:  func(string) string { return legacy.server.URL },
		OwnerManager: mgr,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	t.Cleanup(cancel)
	loop.Run(ctx)

	got := legacy.requests()
	require.NotEmpty(t, got, "unfanned dispatch must be unaffected by the instance-identity gate")
	require.Equal(t, taskID, got[0].TaskID)
	require.Equal(t, uuid.Nil, got[0].TaskRunID)
	require.Zero(t, legacy.probeCount(),
		"an all-unfanned run must not pay for a capability probe")
}
