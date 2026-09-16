//go:build integration

package test

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/cluster"
	"github.com/caesium-cloud/caesium/pkg/env"
	dqliteapp "github.com/canonical/go-dqlite/v3/app"
	"github.com/stretchr/testify/require"
)

// Cluster liveness against a REAL stopped member.
//
// Every other failure-path assertion in this change is synthetic: the Go unit
// tests replace `membershipFunc`/`probeFunc`, and the Playwright scenarios stub
// `/health`. None of them would catch a `liveProbe` that always returned
// success, which is exactly what issue #494 is about — the console reported a
// crashed replica as healthy because nothing ever checked.
//
// This scenario therefore uses NO stubs. It starts three real dqlite nodes,
// forms a real raft cluster, and drives `cluster.Refresh` — the real
// `liveMembership` (a real dqlite RPC) and the real `liveProbe` (a real
// connection to each member) — before stopping a node, after stopping it, and
// after bringing it back.
//
// It runs three real nodes in-process rather than three server containers
// because no three-node lane exists: `just integration-up-distributed` starts a
// SINGLE container in distributed execution mode (one dqlite node at
// 127.0.0.1:9001), and the only harness that runs a real multi-replica cluster
// is the kind robustness lane. Everything below the HTTP layer is real here;
// the HTTP layer over a real server is covered by
// TestSystemHealthReportsProbedQuorum.
func TestClusterLivenessObservesARealStoppedNode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	addrs := []string{freeLoopbackAddr(t), freeLoopbackAddr(t), freeLoopbackAddr(t)}
	dirs := []string{t.TempDir(), t.TempDir(), t.TempDir()}

	nodes := make([]*dqliteapp.App, len(addrs))
	nodes[0] = startDqliteNode(t, ctx, dirs[0], addrs[0], nil)
	nodes[1] = startDqliteNode(t, ctx, dirs[1], addrs[1], addrs[:1])
	nodes[2] = startDqliteNode(t, ctx, dirs[2], addrs[2], addrs[:1])

	stopped := map[int]bool{}
	t.Cleanup(func() {
		for i, n := range nodes {
			if n != nil && !stopped[i] {
				_ = n.Close()
			}
		}
	})

	// Point the liveness reader at the first node, exactly as a server process
	// would be configured.
	t.Setenv("CAESIUM_DATABASE_TYPE", "dqlite")
	t.Setenv("CAESIUM_NODE_ADDRESS", addrs[0])
	require.NoError(t, env.Process())
	t.Cleanup(func() { _ = env.Process() })

	// 1. A healthy cluster: every voter answers a real probe.
	healthy := awaitClusterView(t, ctx, func(v cluster.View) bool {
		return v.Quorum.Status == cluster.StatusAvailable && v.Quorum.TotalVoters == len(addrs)
	}, "three real voters never became available")

	t.Logf("healthy: %+v", healthy.Quorum)
	require.Equal(t, 3, healthy.Quorum.TotalVoters)
	require.Equal(t, 3, healthy.Quorum.ReachableVoters)
	require.Equal(t, 2, healthy.Quorum.RequiredVoters)
	require.True(t, healthy.Quorum.Available)
	require.False(t, healthy.Quorum.Degraded)
	require.Equal(t, cluster.StatusAvailable, healthy.Nodes.Status)
	for _, m := range healthy.Members {
		require.Equal(t, cluster.Reachable, m.Reachability, "member %s", m.Address)
		require.NotNil(t, m.LatencyMs, "member %s answered without a measured probe", m.Address)
	}

	// 2. Stop one member for real. Nothing is mocked: the process listening on
	//    that address goes away.
	require.NoError(t, nodes[2].Close())
	stopped[2] = true

	degraded := awaitClusterView(t, ctx, func(v cluster.View) bool {
		return v.Quorum.ReachableVoters == 2
	}, "the stopped node was never observed as unreachable — a probe that always succeeds would hang here")

	t.Logf("after stopping %s: %+v", addrs[2], degraded.Quorum)
	require.Equal(t, 3, degraded.Quorum.TotalVoters, "membership still lists the stopped member")
	require.Equal(t, 2, degraded.Quorum.ReachableVoters, "availability must not follow membership")
	require.Equal(t, 1, degraded.Quorum.UnreachableVoters)
	require.True(t, degraded.Quorum.Available, "two of three voters still serve")
	require.True(t, degraded.Quorum.Degraded)
	require.Equal(t, cluster.StatusDegraded, degraded.Quorum.Status)
	require.Equal(t, cluster.StatusDegraded, degraded.Nodes.Status)
	require.Equal(t, cluster.StatusDegraded, degraded.Status())

	dead, ok := degraded.MemberOf(addrs[2])
	require.True(t, ok, "the stopped member must still be listed")
	require.Equal(t, cluster.Unreachable, dead.Reachability)
	require.Nil(t, dead.LatencyMs, "an unreachable member cannot have a measured latency")

	for _, addr := range addrs[:2] {
		survivor, ok := degraded.MemberOf(addr)
		require.True(t, ok)
		require.Equal(t, cluster.Reachable, survivor.Reachability, "survivor %s", addr)
	}

	// 3. Bring it back and observe recovery, so the degraded state is not a
	//    one-way latch.
	nodes[2] = startDqliteNode(t, ctx, dirs[2], addrs[2], addrs[:1])
	stopped[2] = false

	recovered := awaitClusterView(t, ctx, func(v cluster.View) bool {
		return v.Quorum.Status == cluster.StatusAvailable
	}, "the restarted node was never observed as reachable again")

	t.Logf("after restarting %s: %+v", addrs[2], recovered.Quorum)
	require.Equal(t, 3, recovered.Quorum.ReachableVoters)
	require.False(t, recovered.Quorum.Degraded)
	require.Equal(t, cluster.StatusAvailable, recovered.Nodes.Status)

	back, ok := recovered.MemberOf(addrs[2])
	require.True(t, ok)
	require.Equal(t, cluster.Reachable, back.Reachability)
}

// awaitClusterView refreshes the real observation until it satisfies want.
func awaitClusterView(
	t *testing.T,
	ctx context.Context,
	want func(cluster.View) bool,
	msg string,
) cluster.View {
	t.Helper()

	deadline := time.Now().Add(90 * time.Second)
	var last cluster.View
	for time.Now().Before(deadline) {
		refreshCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		last = cluster.Refresh(refreshCtx)
		cancel()

		if want(last) {
			return last
		}
		time.Sleep(500 * time.Millisecond)
	}

	t.Fatalf("%s (last observation: clustered=%v observed=%v quorum=%+v members=%+v)",
		msg, last.Clustered, last.Observed, last.Quorum, last.Members)
	return last
}

func startDqliteNode(t *testing.T, ctx context.Context, dir, addr string, seeds []string) *dqliteapp.App {
	t.Helper()

	options := []dqliteapp.Option{dqliteapp.WithAddress(addr)}
	if len(seeds) > 0 {
		options = append(options, dqliteapp.WithCluster(seeds))
	}

	node, err := dqliteapp.New(dir, options...)
	require.NoErrorf(t, err, "start dqlite node %s", addr)

	readyCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	require.NoErrorf(t, node.Ready(readyCtx), "dqlite node %s never became ready", addr)

	return node
}

// freeLoopbackAddr reserves a loopback port and releases it, so the dqlite node
// can bind it. The lanes that run this suite share a network namespace with the
// server container, so a hardcoded port could collide with its dqlite listener.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()

	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())

	return fmt.Sprintf("127.0.0.1:%d", port)
}
