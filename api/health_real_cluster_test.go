package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/cluster"
	"github.com/caesium-cloud/caesium/pkg/env"
	dqliteapp "github.com/canonical/go-dqlite/v3/app"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// Cluster liveness against a REAL stopped member, observed through the REAL
// production path.
//
// Every other failure-path assertion in this change is synthetic: the other Go
// tests replace `membershipFunc`/`probeFunc`, and the Playwright scenarios stub
// `/health`. None of them would catch a `liveProbe` that always returned
// success, which is exactly what issue #494 is about — the console reported a
// crashed replica as healthy because nothing ever checked.
//
// So this scenario stubs nothing about the cluster. It starts three real dqlite
// nodes, forms a real raft cluster, and observes them the way the server does:
//
//   - `cluster.Snapshot` with its BACKGROUND refresh. `cluster.Refresh` is never
//     called, so a regression that populated the cache once and then stopped
//     refreshing would fail here rather than pass.
//   - the real `Health` and `HealthReady` handlers, mounted on a real echo
//     router behind a real HTTP server, asserted from the decoded response body.
//
// Only the database-backed checks are substituted, because `db.Connection()`
// would want to become a fourth dqlite node on the address node one already
// holds. Those checks are covered against a real server by the
// TestSystemHealthReportsProbedQuorum and TestHealthProbeEndpointsAreSplit
// integration scenarios.
//
// Three nodes run in-process rather than as three containers because no
// three-node lane exists: `just integration-up-distributed` starts a SINGLE
// container in distributed execution mode (one dqlite node at 127.0.0.1:9001),
// and the only harness that runs a real multi-replica cluster is the kind
// robustness lane.
func TestHealthObservesARealStoppedNodeOverHTTP(t *testing.T) {
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

	// Configure the process exactly as a server pointed at node one would be.
	t.Setenv("CAESIUM_DATABASE_TYPE", "dqlite")
	t.Setenv("CAESIUM_NODE_ADDRESS", addrs[0])
	require.NoError(t, env.Process())
	t.Cleanup(func() { _ = env.Process() })

	// Short enough that a change propagates within the test, long enough that
	// the refresh is still a background one.
	t.Cleanup(cluster.SetRefreshInterval(200 * time.Millisecond))

	// The REAL handlers, with only the database probes substituted. Note
	// `cluster: cluster.Snapshot` — the production observer, background refresh
	// and all.
	previous := activeHealthChecker
	activeHealthChecker = func() healthChecker {
		return healthChecker{
			database:   func(context.Context) *CheckResult { return &CheckResult{Status: Healthy, LatencyMs: 1} },
			activeRuns: func(context.Context) *CheckResult { return &CheckResult{Status: Healthy} },
			triggers:   func(context.Context) *CheckResult { return &CheckResult{Status: Healthy} },
			workers:    func(context.Context) int64 { return 0 },
			cluster:    cluster.Snapshot,
			uptime:     func() time.Duration { return time.Minute },
			timeout:    databaseCheckTimeout,
		}
	}
	t.Cleanup(func() { activeHealthChecker = previous })

	e := echo.New()
	e.GET("/health", Health)
	e.GET("/health/ready", HealthReady)
	e.GET("/health/live", HealthLive)
	server := httptest.NewServer(e)
	t.Cleanup(server.Close)

	// 1. A healthy cluster, reported over HTTP.
	healthy := awaitHealth(t, ctx, server.URL, func(b healthBody) bool {
		return b.Checks.Cluster != nil &&
			b.Checks.Cluster.Quorum.Status == "available" &&
			b.Checks.Cluster.Quorum.TotalVoters == len(addrs)
	}, "three real voters never became available over HTTP")

	t.Logf("healthy: %+v", healthy.Checks.Cluster.Quorum)
	require.Equal(t, "healthy", healthy.Status)
	require.Equal(t, 3, healthy.Checks.Cluster.Quorum.TotalVoters)
	require.Equal(t, 3, healthy.Checks.Cluster.Quorum.ReachableVoters)
	require.Equal(t, 2, healthy.Checks.Cluster.Quorum.RequiredVoters)
	require.True(t, healthy.Checks.Cluster.Quorum.Available)
	require.False(t, healthy.Checks.Cluster.Quorum.Degraded)
	require.Equal(t, "healthy", healthy.Checks.Nodes.Status)
	require.Len(t, healthy.Checks.Cluster.Members, 3)
	for _, m := range healthy.Checks.Cluster.Members {
		require.Equal(t, "reachable", m.Reachability, "member %s", m.Address)
	}

	// Readiness agrees, and the pod is serving.
	readyCode, ready := getHealth(t, ctx, server.URL+"/health/ready")
	require.Equal(t, http.StatusOK, readyCode)
	require.NotNil(t, ready.Checks.Cluster)

	// 2. Stop one member for real. Nothing is mocked: the process listening on
	//    that address goes away, and the background refresh has to notice.
	require.NoError(t, nodes[2].Close())
	stopped[2] = true

	degraded := awaitHealth(t, ctx, server.URL, func(b healthBody) bool {
		return b.Checks.Cluster != nil && b.Checks.Cluster.Quorum.ReachableVoters == 2
	}, "the stopped node was never reported unreachable over HTTP — a probe that always succeeds, or a snapshot that stops refreshing, would hang here")

	t.Logf("after stopping %s: %+v", addrs[2], degraded.Checks.Cluster.Quorum)
	require.Equal(t, "degraded", degraded.Status)
	require.Equal(t, 3, degraded.Checks.Cluster.Quorum.TotalVoters, "membership still lists the stopped member")
	require.Equal(t, 2, degraded.Checks.Cluster.Quorum.ReachableVoters, "availability must not follow membership")
	require.Equal(t, 1, degraded.Checks.Cluster.Quorum.UnreachableVoters)
	require.True(t, degraded.Checks.Cluster.Quorum.Available, "two of three voters still serve")
	require.True(t, degraded.Checks.Cluster.Quorum.Degraded)
	require.Equal(t, "degraded", degraded.Checks.Cluster.Status)
	require.Equal(t, "degraded", degraded.Checks.Nodes.Status)

	var dead, alive int
	for _, m := range degraded.Checks.Cluster.Members {
		switch m.Reachability {
		case "unreachable":
			dead++
			require.Equal(t, addrs[2], m.Address)
		case "reachable":
			alive++
		}
	}
	require.Equal(t, 1, dead, "exactly the stopped member must be unreachable")
	require.Equal(t, 2, alive)

	// The surviving replica can still serve, so readiness must not fail it.
	degradedCode, _ := getHealth(t, ctx, server.URL+"/health/ready")
	require.Equal(t, http.StatusOK, degradedCode,
		"a peer being down must not take a serviceable replica out of the Service")

	// 3. Bring it back and observe recovery, so degraded is not a one-way latch.
	nodes[2] = startDqliteNode(t, ctx, dirs[2], addrs[2], addrs[:1])
	stopped[2] = false

	recovered := awaitHealth(t, ctx, server.URL, func(b healthBody) bool {
		return b.Checks.Cluster != nil && b.Checks.Cluster.Quorum.Status == "available"
	}, "the restarted node was never reported reachable again over HTTP")

	t.Logf("after restarting %s: %+v", addrs[2], recovered.Checks.Cluster.Quorum)
	require.Equal(t, "healthy", recovered.Status)
	require.Equal(t, 3, recovered.Checks.Cluster.Quorum.ReachableVoters)
	require.False(t, recovered.Checks.Cluster.Quorum.Degraded)
	require.Equal(t, "healthy", recovered.Checks.Nodes.Status)
}

// awaitHealth polls the real /health endpoint until the decoded body satisfies
// want. It never calls cluster.Refresh: propagation has to come from the
// background refresh the server relies on.
func awaitHealth(
	t *testing.T,
	ctx context.Context,
	baseURL string,
	want func(healthBody) bool,
	msg string,
) healthBody {
	t.Helper()

	deadline := time.Now().Add(90 * time.Second)
	var last healthBody
	for time.Now().Before(deadline) {
		_, last = getHealth(t, ctx, baseURL+"/health")
		if want(last) {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}

	raw, _ := json.Marshal(last)
	t.Fatalf("%s (last response: %s)", msg, raw)
	return last
}

func getHealth(t *testing.T, ctx context.Context, url string) (int, healthBody) {
	t.Helper()

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var body healthBody
	require.NoError(t, json.Unmarshal(raw, &body), string(raw))
	return resp.StatusCode, body
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
// can bind it.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()

	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())

	return fmt.Sprintf("127.0.0.1:%d", port)
}
