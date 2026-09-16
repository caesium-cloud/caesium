package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
	"github.com/stretchr/testify/require"
)

func voter(addr string, r Reachability) Member {
	return Member{Address: addr, Role: RoleVoter, Reachability: r}
}

// TestSummarizeDecisionTable pins the meaning of "available", "degraded",
// "unavailable" and "unknown". Issue #494: a cluster that lost a replica must
// never summarise as fully available, and liveness that was never observed must
// never summarise as healthy.
func TestSummarizeDecisionTable(t *testing.T) {
	cases := []struct {
		name            string
		members         []Member
		wantStatus      Status
		wantAvailable   bool
		wantDegraded    bool
		wantReachable   int
		wantTotal       int
		wantRequired    int
		wantUnknown     int
		wantUnreachable int
	}{
		{
			name:          "all voters reachable",
			members:       []Member{voter("a:9001", Reachable), voter("b:9001", Reachable), voter("c:9001", Reachable)},
			wantStatus:    StatusAvailable,
			wantAvailable: true,
			wantDegraded:  false,
			wantReachable: 3, wantTotal: 3, wantRequired: 2,
		},
		{
			name:          "one voter unreachable still has quorum but is degraded",
			members:       []Member{voter("a:9001", Reachable), voter("b:9001", Reachable), voter("c:9001", Unreachable)},
			wantStatus:    StatusDegraded,
			wantAvailable: true,
			wantDegraded:  true,
			wantReachable: 2, wantTotal: 3, wantRequired: 2, wantUnreachable: 1,
		},
		{
			name:          "two of three voters unreachable loses quorum",
			members:       []Member{voter("a:9001", Reachable), voter("b:9001", Unreachable), voter("c:9001", Unreachable)},
			wantStatus:    StatusUnavailable,
			wantAvailable: false,
			wantDegraded:  true,
			wantReachable: 1, wantTotal: 3, wantRequired: 2, wantUnreachable: 2,
		},
		{
			name:          "unknown liveness is never available",
			members:       []Member{voter("a:9001", Unknown), voter("b:9001", Unknown), voter("c:9001", Unknown)},
			wantStatus:    StatusUnknown,
			wantAvailable: false,
			wantDegraded:  true,
			wantReachable: 0, wantTotal: 3, wantRequired: 2, wantUnknown: 3,
		},
		{
			name:          "one reachable plus unknowns that could close the gap is unknown, not available",
			members:       []Member{voter("a:9001", Reachable), voter("b:9001", Unknown), voter("c:9001", Unreachable)},
			wantStatus:    StatusUnknown,
			wantAvailable: false,
			wantDegraded:  true,
			wantReachable: 1, wantTotal: 3, wantRequired: 2, wantUnknown: 1, wantUnreachable: 1,
		},
		{
			name:          "majority reachable with one unknown is degraded, not available",
			members:       []Member{voter("a:9001", Reachable), voter("b:9001", Reachable), voter("c:9001", Unknown)},
			wantStatus:    StatusDegraded,
			wantAvailable: true,
			wantDegraded:  true,
			wantReachable: 2, wantTotal: 3, wantRequired: 2, wantUnknown: 1,
		},
		{
			name:          "single node cluster is available when it answers",
			members:       []Member{voter("a:9001", Reachable)},
			wantStatus:    StatusAvailable,
			wantAvailable: true,
			wantReachable: 1, wantTotal: 1, wantRequired: 1,
		},
		{
			name:          "single node cluster that does not answer is unavailable",
			members:       []Member{voter("a:9001", Unreachable)},
			wantStatus:    StatusUnavailable,
			wantAvailable: false,
			wantDegraded:  true,
			wantReachable: 0, wantTotal: 1, wantRequired: 1, wantUnreachable: 1,
		},
		{
			name: "standby and spare members do not count towards quorum",
			members: []Member{
				voter("a:9001", Reachable),
				{Address: "b:9001", Role: RoleStandby, Reachability: Reachable},
				{Address: "c:9001", Role: RoleSpare, Reachability: Reachable},
			},
			wantStatus:    StatusAvailable,
			wantAvailable: true,
			wantReachable: 1, wantTotal: 1, wantRequired: 1,
		},
		{
			name:       "no voters at all is unknown",
			members:    []Member{{Address: "b:9001", Role: RoleStandby, Reachability: Reachable}},
			wantStatus: StatusUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := Summarize(tc.members, "a:9001")

			require.Equal(t, tc.wantStatus, q.Status)
			require.Equal(t, tc.wantAvailable, q.Available, "available")
			require.Equal(t, tc.wantDegraded, q.Degraded, "degraded")
			require.Equal(t, tc.wantReachable, q.ReachableVoters, "reachable_voters")
			require.Equal(t, tc.wantTotal, q.TotalVoters, "total_voters")
			require.Equal(t, tc.wantRequired, q.RequiredVoters, "required_voters")
			require.Equal(t, tc.wantUnknown, q.UnknownVoters, "unknown_voters")
			require.Equal(t, tc.wantUnreachable, q.UnreachableVoters, "unreachable_voters")
			require.Equal(t, "a:9001", q.LeaderAddress)
		})
	}
}

// TestSummarizeNeverReportsFullQuorumFromMembershipAlone is the regression for
// the reported symptom: three configured members, one of them dead, previously
// rendered as "3/3".
func TestSummarizeNeverReportsFullQuorumFromMembershipAlone(t *testing.T) {
	members := []Member{
		voter("10.244.0.8:9001", Reachable),
		voter("10.244.0.9:9001", Reachable),
		voter("10.244.0.10:9001", Unreachable), // the crashed replica's stale IP
	}

	q := Summarize(members, "10.244.0.8:9001")

	require.Equal(t, 3, q.TotalVoters)
	require.Equal(t, 2, q.ReachableVoters)
	require.NotEqual(t, q.TotalVoters, q.ReachableVoters)
	require.True(t, q.Available, "two of three voters still serve")
	require.True(t, q.Degraded)
	require.Equal(t, StatusDegraded, q.Status)
}

// TestSummarizeNodesCoversNonVoters pins the complement to the quorum table:
// quorum ignores standbys and spares by design, so node liveness must not.
func TestSummarizeNodesCoversNonVoters(t *testing.T) {
	standby := func(addr string, r Reachability) Member {
		return Member{Address: addr, Role: RoleStandby, Reachability: r}
	}
	spare := func(addr string, r Reachability) Member {
		return Member{Address: addr, Role: RoleSpare, Reachability: r}
	}

	cases := []struct {
		name            string
		members         []Member
		wantStatus      Status
		wantTotal       int
		wantReachable   int
		wantUnreachable int
		wantUnknown     int
	}{
		{
			name:       "every member reachable",
			members:    []Member{voter("a:9001", Reachable), standby("b:9001", Reachable)},
			wantStatus: StatusAvailable, wantTotal: 2, wantReachable: 2,
		},
		{
			name:       "an unreachable standby degrades node health",
			members:    []Member{voter("a:9001", Reachable), voter("b:9001", Reachable), voter("c:9001", Reachable), standby("d:9001", Unreachable)},
			wantStatus: StatusDegraded, wantTotal: 4, wantReachable: 3, wantUnreachable: 1,
		},
		{
			name:       "an unreachable spare degrades node health",
			members:    []Member{voter("a:9001", Reachable), spare("d:9001", Unreachable)},
			wantStatus: StatusDegraded, wantTotal: 2, wantReachable: 1, wantUnreachable: 1,
		},
		{
			name:       "an unverified standby is unknown, never healthy",
			members:    []Member{voter("a:9001", Reachable), standby("b:9001", Unknown)},
			wantStatus: StatusUnknown, wantTotal: 2, wantReachable: 1, wantUnknown: 1,
		},
		{
			name:       "a known failure outranks an unverified member",
			members:    []Member{voter("a:9001", Unreachable), standby("b:9001", Unknown)},
			wantStatus: StatusDegraded, wantTotal: 2, wantUnreachable: 1, wantUnknown: 1,
		},
		{
			name:       "no members at all is unknown",
			members:    nil,
			wantStatus: StatusUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := SummarizeNodes(tc.members)

			require.Equal(t, tc.wantStatus, s.Status)
			require.Equal(t, tc.wantTotal, s.Total, "total")
			require.Equal(t, tc.wantReachable, s.Reachable, "reachable")
			require.Equal(t, tc.wantUnreachable, s.Unreachable, "unreachable")
			require.Equal(t, tc.wantUnknown, s.Unknown, "unknown")
		})
	}
}

// TestViewStatusIsTheWorseOfQuorumAndNodes is the wiring between the two
// summaries: an intact voter majority must not mask a dead non-voter.
func TestViewStatusIsTheWorseOfQuorumAndNodes(t *testing.T) {
	members := []Member{
		voter("a:9001", Reachable),
		voter("b:9001", Reachable),
		voter("c:9001", Reachable),
		{Address: "d:9001", Role: RoleStandby, Reachability: Unreachable},
	}
	view := View{
		Clustered: true,
		Observed:  true,
		Members:   members,
		Quorum:    Summarize(members, "a:9001"),
		Nodes:     SummarizeNodes(members),
	}

	require.Equal(t, StatusAvailable, view.Quorum.Status, "quorum arithmetic stays voter-only")
	require.Equal(t, StatusDegraded, view.Nodes.Status)
	require.Equal(t, StatusDegraded, view.Status())

	// A lost quorum still outranks a merely degraded node count.
	lost := []Member{voter("a:9001", Reachable), voter("b:9001", Unreachable), voter("c:9001", Unreachable)}
	lostView := View{Quorum: Summarize(lost, ""), Nodes: SummarizeNodes(lost)}
	require.Equal(t, StatusUnavailable, lostView.Status())
}

func TestObserveSummarizesNonVoterLiveness(t *testing.T) {
	restore := stub(t,
		func(context.Context) ([]client.NodeInfo, string, error) {
			return []client.NodeInfo{
				{ID: 1, Address: "10.244.0.8:9001", Role: client.Voter},
				{ID: 2, Address: "10.244.0.9:9001", Role: client.Voter},
				{ID: 3, Address: "10.244.0.10:9001", Role: client.Voter},
				{ID: 4, Address: "10.244.0.11:9001", Role: client.StandBy},
			}, "10.244.0.8:9001", nil
		},
		func(_ context.Context, addr string) error {
			if addr == "10.244.0.11:9001" {
				return errors.New("connection refused")
			}
			return nil
		},
	)
	defer restore()

	view := observe(context.Background())

	require.Equal(t, StatusAvailable, view.Quorum.Status)
	require.Equal(t, 3, view.Quorum.TotalVoters)
	require.Equal(t, StatusDegraded, view.Nodes.Status)
	require.Equal(t, 4, view.Nodes.Total)
	require.Equal(t, 1, view.Nodes.Unreachable)
	require.Equal(t, StatusDegraded, view.Status())

	standby, ok := view.MemberOf("10.244.0.11:9001")
	require.True(t, ok)
	require.Equal(t, RoleStandby, standby.Role)
	require.Equal(t, Unreachable, standby.Reachability)
}

func TestObserveProbesEveryMemberAndMarksTheLeader(t *testing.T) {
	restore := stub(t,
		func(context.Context) ([]client.NodeInfo, string, error) {
			return []client.NodeInfo{
				{ID: 1, Address: "10.244.0.8:9001", Role: client.Voter},
				{ID: 2, Address: "10.244.0.9:9001", Role: client.Voter},
				{ID: 3, Address: "10.244.0.10:9001", Role: client.Voter},
			}, "10.244.0.8:9001", nil
		},
		func(_ context.Context, addr string) error {
			if addr == "10.244.0.10:9001" {
				return errors.New("connection refused")
			}
			return nil
		},
	)
	defer restore()

	view := observe(context.Background())

	require.True(t, view.Clustered)
	require.True(t, view.Observed)
	require.Len(t, view.Members, 3)
	require.Equal(t, StatusDegraded, view.Quorum.Status)
	require.Equal(t, 2, view.Quorum.ReachableVoters)
	require.Equal(t, 3, view.Quorum.TotalVoters)

	leader, ok := view.MemberOf("10.244.0.8:9001")
	require.True(t, ok)
	require.True(t, leader.Leader)
	require.Equal(t, Reachable, leader.Reachability)
	require.NotNil(t, leader.LatencyMs)

	dead, ok := view.MemberOf("10.244.0.10:9001")
	require.True(t, ok)
	require.False(t, dead.Leader)
	require.Equal(t, Unreachable, dead.Reachability)
	require.Nil(t, dead.LatencyMs)
}

// TestObserveProbesEveryMemberBeyondTheConcurrencyCap: the cap must bound
// PARALLELISM, not coverage. An earlier version truncated the member list at
// the cap, so on a larger membership the members past it were never probed and
// stayed Unknown forever — and unknown liveness is never healthy, so such a
// deployment could never report healthy.
func TestObserveProbesEveryMemberBeyondTheConcurrencyCap(t *testing.T) {
	const total = maxConcurrentProbes + 8

	infos := make([]client.NodeInfo, 0, total)
	for i := range total {
		infos = append(infos, client.NodeInfo{
			ID: uint64(i + 1),
			// Zero-padded so lexical sorting cannot coincidentally place the
			// members this test cares about inside the first wave.
			Address: fmt.Sprintf("10.0.0.%03d:9001", i+1),
			Role:    client.Voter,
		})
	}

	var (
		mu       sync.Mutex
		probed   = map[string]int{}
		inFlight int
		peak     int
	)
	restore := stub(t,
		func(context.Context) ([]client.NodeInfo, string, error) {
			return infos, infos[0].Address, nil
		},
		func(_ context.Context, addr string) error {
			mu.Lock()
			probed[addr]++
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			mu.Unlock()

			time.Sleep(2 * time.Millisecond)

			mu.Lock()
			inFlight--
			mu.Unlock()
			return nil
		},
	)
	defer restore()

	view := observe(context.Background())

	require.Len(t, view.Members, total)
	for _, info := range infos {
		require.Equal(t, 1, probed[info.Address], "member %s was not probed exactly once", info.Address)
	}
	for _, m := range view.Members {
		require.Equal(t, Reachable, m.Reachability, "member %s was left unprobed", m.Address)
	}

	require.Equal(t, total, view.Quorum.TotalVoters)
	require.Equal(t, total, view.Quorum.ReachableVoters)
	require.Equal(t, StatusAvailable, view.Quorum.Status, "a large membership must be able to report healthy")
	require.Equal(t, StatusAvailable, view.Nodes.Status)
	require.Zero(t, view.Nodes.Unknown)

	mu.Lock()
	defer mu.Unlock()
	require.LessOrEqual(t, peak, maxConcurrentProbes, "probe concurrency exceeded the cap")
}

func TestObserveWithUnavailableMembershipReportsUnknownNotHealthy(t *testing.T) {
	restore := stub(t,
		func(context.Context) ([]client.NodeInfo, string, error) {
			return nil, "", errors.New("dial tcp: connection refused")
		},
		func(context.Context, string) error { return nil },
	)
	defer restore()

	view := observe(context.Background())

	require.True(t, view.Observed)
	require.Equal(t, StatusUnknown, view.Quorum.Status)
	require.False(t, view.Quorum.Available)
	for _, m := range view.Members {
		require.Equal(t, Unknown, m.Reachability, "seeded member %s must not be presented as healthy", m.Address)
	}
}

func TestSnapshotReturnsUnknownBeforeAnyObservationAndDoesNotBlock(t *testing.T) {
	probed := make(chan struct{})
	restore := stub(t,
		func(context.Context) ([]client.NodeInfo, string, error) {
			return []client.NodeInfo{{ID: 1, Address: "a:9001", Role: client.Voter}}, "a:9001", nil
		},
		func(context.Context, string) error {
			close(probed)
			return nil
		},
	)
	defer restore()

	first := Snapshot()
	require.False(t, first.Observed, "no probe has completed yet")
	require.Equal(t, StatusUnknown, first.Quorum.Status)
	require.False(t, first.Quorum.Available)

	select {
	case <-probed:
	case <-time.After(5 * time.Second):
		t.Fatal("Snapshot did not schedule a background refresh")
	}

	require.Eventually(t, func() bool {
		return Snapshot().Observed
	}, 5*time.Second, 10*time.Millisecond)

	warm := Snapshot()
	require.Equal(t, StatusAvailable, warm.Quorum.Status)
	require.True(t, warm.Quorum.Available)
	require.False(t, warm.Stale)
}

// TestSnapshotRefreshesAnAlreadyObservedView: an observation that has already
// been taken must keep being retaken. A regression that only ever populated the
// cache once would still satisfy every "first observation" assertion, and the
// console would then show a snapshot frozen at the moment the cluster was last
// healthy — which is the failure mode issue #494 reported in the first place.
func TestSnapshotRefreshesAnAlreadyObservedView(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)

	restore := stub(t,
		func(context.Context) ([]client.NodeInfo, string, error) {
			return []client.NodeInfo{
				{ID: 1, Address: "a:9001", Role: client.Voter},
				{ID: 2, Address: "b:9001", Role: client.Voter},
				{ID: 3, Address: "c:9001", Role: client.Voter},
			}, "a:9001", nil
		},
		func(_ context.Context, addr string) error {
			if addr == "c:9001" && !healthy.Load() {
				return errors.New("connection refused")
			}
			return nil
		},
	)
	defer restore()
	defer SetRefreshInterval(10 * time.Millisecond)()

	require.Eventually(t, func() bool {
		return Snapshot().Quorum.Status == StatusAvailable
	}, 5*time.Second, 10*time.Millisecond, "the first observation never completed")

	// The cluster changes underneath an ALREADY observed snapshot.
	healthy.Store(false)

	require.Eventually(t, func() bool {
		v := Snapshot()
		return v.Quorum.Status == StatusDegraded && v.Quorum.ReachableVoters == 2
	}, 5*time.Second, 10*time.Millisecond, "an already-observed snapshot was never refreshed")

	degraded := Snapshot()
	dead, ok := degraded.MemberOf("c:9001")
	require.True(t, ok)
	require.Equal(t, Unreachable, dead.Reachability)

	// ...and back again, so the refresh is not a one-way latch.
	healthy.Store(true)

	require.Eventually(t, func() bool {
		return Snapshot().Quorum.Status == StatusAvailable
	}, 5*time.Second, 10*time.Millisecond, "recovery was never observed")
}

func TestSnapshotOnNonDqliteDeploymentIsNotClustered(t *testing.T) {
	reset()
	prev := clusteredFunc
	clusteredFunc = func() bool { return false }
	t.Cleanup(func() {
		reset()
		clusteredFunc = prev
	})

	view := Refresh(context.Background())

	require.False(t, view.Clustered)
	require.Empty(t, view.Members)
	require.Equal(t, StatusUnknown, view.Quorum.Status)
}

// TestViewJSONContract pins the wire field names the console reads.
func TestViewJSONContract(t *testing.T) {
	latency := int64(3)
	view := View{
		Clustered:  true,
		Observed:   true,
		ObservedAt: time.Unix(0, 0).UTC(),
		Members: []Member{{
			ID: 1, Address: "a:9001", Role: RoleVoter, Leader: true,
			Reachability: Reachable, LatencyMs: &latency,
		}},
		Quorum: Summarize([]Member{voter("a:9001", Reachable)}, "a:9001"),
		Nodes:  SummarizeNodes([]Member{voter("a:9001", Reachable)}),
	}

	raw, err := json.Marshal(view)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Contains(t, decoded, "quorum")
	require.Contains(t, decoded, "members")
	require.Contains(t, decoded, "nodes")

	nodes, ok := decoded["nodes"].(map[string]any)
	require.True(t, ok)
	for _, field := range []string{"status", "total", "reachable", "unreachable", "unknown"} {
		require.Contains(t, nodes, field)
	}

	quorum, ok := decoded["quorum"].(map[string]any)
	require.True(t, ok)
	for _, field := range []string{
		"status", "total_voters", "reachable_voters", "unreachable_voters",
		"unknown_voters", "required_voters", "available", "degraded", "leader_address",
	} {
		require.Contains(t, quorum, field)
	}

	member, ok := decoded["members"].([]any)[0].(map[string]any)
	require.True(t, ok)
	for _, field := range []string{"address", "role", "leader", "reachability", "latency_ms"} {
		require.Contains(t, member, field)
	}
}

func stub(
	t *testing.T,
	membership func(context.Context) ([]client.NodeInfo, string, error),
	probe func(context.Context, string) error,
) func() {
	t.Helper()
	reset()

	prevMembership, prevProbe, prevClustered := membershipFunc, probeFunc, clusteredFunc
	membershipFunc = membership
	probeFunc = probe
	clusteredFunc = func() bool { return true }

	return func() {
		// reset() first: it waits for any in-flight background refresh, so the
		// restore below cannot race the goroutine still reading these vars.
		reset()
		membershipFunc = prevMembership
		probeFunc = prevProbe
		clusteredFunc = prevClustered
	}
}
