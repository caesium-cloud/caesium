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
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// peerStub is a fake node that records what the dispatch loop asked of it.
type peerStub struct {
	server *httptest.Server

	mu         sync.Mutex
	dispatched []DispatchRequest
	probes     int
}

// newPeerStub builds a stub that serves /internal/dispatch and 404s
// /internal/capabilities — the same shape any peer has always had, since
// nothing on the dispatch path probes capabilities any more (#358).
func newPeerStub(t *testing.T) *peerStub {
	t.Helper()
	p := &peerStub{}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/capabilities":
			p.mu.Lock()
			p.probes++
			p.mu.Unlock()
			http.NotFound(w, r)
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

// TestDispatchRunInMemory_FannedInstancesDispatchWithoutCapabilityProbe pins
// the removal of the rolling-upgrade instance_identity capability gate
// (#358): every fan-out instance now dispatches to whichever peer round-robin
// selects, the same as unfanned work, with no pre-flight
// GET /internal/capabilities probe gating the decision.
func TestDispatchRunInMemory_FannedInstancesDispatchWithoutCapabilityProbe(t *testing.T) {
	const (
		selfNodeID = "10.0.0.1:9001"
		peerNodeID = "10.0.0.2:9001"
	)
	peer1 := newPeerStub(t)

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)

	runID, taskID, instanceIDs := seedFannedRun(t, db, store)
	_, err := ls.AcquireLease(context.Background(), runID, selfNodeID, 30*time.Second)
	require.NoError(t, err)

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
		Peers:      &testPeerLister{peers: []string{peerNodeID}},
		PeerBaseURL: func(addr string) string {
			if addr == peerNodeID {
				return peer1.server.URL
			}
			// selfNodeID resolves to nothing reachable, so every instance must
			// route to peer1 via round-robin — proving self is not required to
			// answer a capability probe for the instance to go out.
			return ""
		},
		OwnerManager: mgr,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	t.Cleanup(cancel)
	loop.Run(ctx)

	require.Zero(t, peer1.probeCount(),
		"a fan-out instance must dispatch without the owner ever probing peer capabilities")

	got := peer1.requests()
	require.Len(t, got, 3, "every instance must be dispatched with no capability gate to satisfy")
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
