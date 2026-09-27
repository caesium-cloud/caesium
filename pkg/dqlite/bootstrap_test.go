package dqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	godqlite "github.com/canonical/go-dqlite/v3"
	dqliteapp "github.com/canonical/go-dqlite/v3/app"
	"github.com/canonical/go-dqlite/v3/client"
	"github.com/stretchr/testify/require"
)

// Roles are adjusted by the leader on a timer (30 s by default). The tests
// shorten it so a joined spare is promoted, and an offline voter demoted,
// within the test's budget.
const testRolesAdjustment = time.Second

func startTunedNode(t *testing.T, ctx context.Context, dir, address string, cluster []string) *dqliteapp.App {
	t.Helper()
	opts := []dqliteapp.Option{dqliteapp.WithAddress(address), dqliteapp.WithRolesAdjustmentFrequency(testRolesAdjustment)}
	if len(cluster) > 0 {
		opts = append(opts, dqliteapp.WithCluster(cluster))
	}
	app, err := dqliteapp.New(dir, opts...)
	require.NoError(t, err)
	readyCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	require.NoError(t, app.Ready(readyCtx))
	return app
}

// openFreshNode starts an empty data directory the way cmd/start does, with the
// given seeds and bootstrap peers.
func openFreshNode(t *testing.T, ctx context.Context, dir, address string, seeds, bootstrapPeers []string) (*dqliteapp.App, error) {
	t.Helper()
	opts := []dqliteapp.Option{dqliteapp.WithAddress(address), dqliteapp.WithRolesAdjustmentFrequency(testRolesAdjustment)}
	if len(seeds) > 0 {
		opts = append(opts, dqliteapp.WithCluster(seeds))
	}
	openCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	return openNativeApp(openCtx, dir, address, seeds, bootstrapPeers, opts...)
}

func shortenBootstrapProbe(t *testing.T, window, interval time.Duration) {
	t.Helper()
	oldWindow, oldInterval := bootstrapProbeWindow, bootstrapProbeInterval
	bootstrapProbeWindow, bootstrapProbeInterval = window, interval
	t.Cleanup(func() { bootstrapProbeWindow, bootstrapProbeInterval = oldWindow, oldInterval })
}

func membershipView(members []client.NodeInfo) string {
	view := make([]string, 0, len(members))
	for _, m := range members {
		view = append(view, fmt.Sprintf("%d/%s/%s", m.ID, m.Address, m.Role))
	}
	sort.Strings(view)
	return strings.Join(view, ",")
}

func localMembers(ctx context.Context, app *dqliteapp.App) ([]client.NodeInfo, error) {
	cli, err := app.Client(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cli.Close() }()
	return cli.Cluster(ctx)
}

func leaderMembers(ctx context.Context, app *dqliteapp.App) (map[uint64]client.NodeInfo, []client.NodeInfo, error) {
	cli, err := app.FindLeader(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = cli.Close() }()
	members, err := cli.Cluster(ctx)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[uint64]client.NodeInfo, len(members))
	for _, m := range members {
		byID[m.ID] = m
	}
	return byID, members, nil
}

// TestFreshOrdinalZeroJoinsLiveClusterThroughBootstrapPeers is issue #582: the
// cluster's bootstrap member loses its disk while the other two keep running.
// With no seeds, go-dqlite would bootstrap a second single-member cluster under
// BootstrapID — the ID the real cluster still lists. The bootstrap peers must
// make it join as a new member instead.
func TestFreshOrdinalZeroJoinsLiveClusterThroughBootstrapPeers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	dirs := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	addrs := []string{"127.0.0.1:9511", "127.0.0.1:9512", "127.0.0.1:9513"}
	apps := make([]*dqliteapp.App, len(dirs))
	defer func() {
		for _, app := range apps {
			if app != nil {
				_ = app.Close()
			}
		}
	}()
	for idx := range dirs {
		var seeds []string
		if idx > 0 {
			seeds = addrs[:idx]
		}
		apps[idx] = startTunedNode(t, ctx, dirs[idx], addrs[idx], seeds)
	}
	require.Equal(t, uint64(godqlite.BootstrapID), apps[0].ID())

	// All three must be voters so that losing ordinal 0 leaves a quorum.
	require.Eventually(t, func() bool {
		byID, _, err := leaderMembers(ctx, apps[1])
		if err != nil || len(byID) != 3 {
			return false
		}
		for _, m := range byID {
			if m.Role != client.Voter {
				return false
			}
		}
		return true
	}, 60*time.Second, 500*time.Millisecond, "three-voter cluster never formed")

	db, err := apps[1].Open(ctx, "test")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	_, err = db.ExecContext(ctx, "CREATE TABLE durable (n INT)")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "INSERT INTO durable VALUES (7)")
	require.NoError(t, err)

	// Disk loss: ordinal 0 stops and comes back with an empty data directory at
	// a new address, knowing only its peers.
	require.NoError(t, apps[0].Close())
	apps[0] = nil

	freshAddr := "127.0.0.2:9511"
	fresh, err := openFreshNode(t, ctx, t.TempDir(), freshAddr, nil, addrs[1:])
	require.NoError(t, err)
	apps[0] = fresh

	require.NotEqual(t, uint64(godqlite.BootstrapID), fresh.ID(), "a replaced member must never reuse the bootstrap ID")

	// The fresh node is in the leader's membership at its own address and is
	// eventually a voter, while the lost member stays listed at its old
	// address, demoted to a spare.
	require.Eventually(t, func() bool {
		byID, _, err := leaderMembers(ctx, apps[1])
		if err != nil {
			return false
		}
		self, okSelf := byID[fresh.ID()]
		stale, okStale := byID[godqlite.BootstrapID]
		return okSelf && okStale && self.Address == freshAddr && self.Role == client.Voter &&
			stale.Address == addrs[0] && stale.Role == client.Spare
	}, 90*time.Second, 500*time.Millisecond, "fresh node never promoted or stale voter never demoted")

	// The fresh node's own committed configuration is the cluster's, not a
	// divergent single-member one.
	require.Eventually(t, func() bool {
		_, leaderView, err := leaderMembers(ctx, apps[1])
		if err != nil {
			return false
		}
		ownView, err := localMembers(ctx, fresh)
		return err == nil && membershipView(ownView) == membershipView(leaderView)
	}, 60*time.Second, 500*time.Millisecond, "fresh node's own membership view diverges from the leader's")

	freshDB, err := fresh.Open(ctx, "test")
	require.NoError(t, err)
	defer func() { _ = freshDB.Close() }()
	var n int
	require.NoError(t, freshDB.QueryRowContext(ctx, "SELECT n FROM durable").Scan(&n))
	require.Equal(t, 7, n)
}

// TestFreshNodeWithSeedsJoins pins the go-dqlite behaviour the chart relies on
// when its init container finds a live peer: seeds on an empty data directory
// mean a fresh, non-bootstrap ID and a join.
func TestFreshNodeWithSeedsJoins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	leader := startTunedNode(t, ctx, t.TempDir(), "127.0.0.1:9521", nil)
	defer func() { _ = leader.Close() }()

	joined, err := openFreshNode(t, ctx, t.TempDir(), "127.0.0.1:9522", []string{"127.0.0.1:9521"}, nil)
	require.NoError(t, err)
	defer func() { _ = joined.Close() }()

	require.NotEqual(t, uint64(godqlite.BootstrapID), joined.ID())
	byID, _, err := leaderMembers(ctx, leader)
	require.NoError(t, err)
	require.Len(t, byID, 2)
	require.Equal(t, "127.0.0.1:9522", byID[joined.ID()].Address)
}

// TestFreshNodeBootstrapsWhenNoBootstrapPeerAnswers covers a new install: the
// first member's peers do not exist yet, so after the bounded probe window it
// bootstraps exactly as go-dqlite always has.
func TestFreshNodeBootstrapsWhenNoBootstrapPeerAnswers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	shortenBootstrapProbe(t, 1500*time.Millisecond, 250*time.Millisecond)

	started := time.Now()
	app, err := openFreshNode(t, ctx, t.TempDir(), "127.0.0.1:9531", nil,
		[]string{"127.0.0.1:9532", "127.0.0.1:9533"})
	require.NoError(t, err)
	defer func() { _ = app.Close() }()

	require.GreaterOrEqual(t, time.Since(started), time.Second, "bootstrapped without waiting out the probe window")
	require.Equal(t, uint64(godqlite.BootstrapID), app.ID())
	members, err := localMembers(ctx, app)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Equal(t, "127.0.0.1:9531", members[0].Address)
}

// TestNodeWithIdentityIgnoresBootstrapPeers keeps an ordinary restart free: a
// node that already has info.yaml, or that has seeds, never probes.
func TestNodeWithIdentityIgnoresBootstrapPeers(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, infoFileName), "ID: 42\nAddress: 127.0.0.1:9541\nRole: 0\n")
	shortenBootstrapProbe(t, time.Minute, time.Second)

	started := time.Now()
	require.Nil(t, freshNodeJoinSeeds(context.Background(), dir, "127.0.0.1:9541", nil, []string{"127.0.0.1:9542"}))
	require.Nil(t, freshNodeJoinSeeds(context.Background(), t.TempDir(), "127.0.0.1:9541",
		[]string{"127.0.0.1:9542"}, []string{"127.0.0.1:9543"}))
	require.Less(t, time.Since(started), time.Second)
}
