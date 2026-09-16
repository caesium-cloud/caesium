package dqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	dqliteapp "github.com/canonical/go-dqlite/v3/app"
	"github.com/canonical/go-dqlite/v3/client"
	"github.com/stretchr/testify/require"
)

func writeYAML(t *testing.T, path string, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
}

func readStore(t *testing.T, dir string) []client.NodeInfo {
	t.Helper()
	store, err := client.NewYamlNodeStore(filepath.Join(dir, clusterFileName))
	require.NoError(t, err)
	members, err := store.Get(context.Background())
	require.NoError(t, err)
	return members
}

func TestReconcilePersistedNodeAddressIsANoop(t *testing.T) {
	t.Run("uninitialized data directory", func(t *testing.T) {
		migration, err := reconcilePersistedNodeAddress(t.TempDir(), "10.0.0.1:9001", nil)
		require.NoError(t, err)
		require.Nil(t, migration)
	})

	t.Run("address unchanged", func(t *testing.T) {
		dir := t.TempDir()
		writeYAML(t, filepath.Join(dir, infoFileName), "ID: 42\nAddress: 10.0.0.1:9001\nRole: 0\n")
		writeYAML(t, filepath.Join(dir, clusterFileName), "- ID: 42\n  Address: 10.0.0.1:9001\n  Role: 0\n")

		migration, err := reconcilePersistedNodeAddress(dir, "10.0.0.1:9001", nil)
		require.NoError(t, err)
		require.Nil(t, migration)
	})

	t.Run("no configured address", func(t *testing.T) {
		dir := t.TempDir()
		writeYAML(t, filepath.Join(dir, infoFileName), "ID: 42\nAddress: 10.0.0.1:9001\nRole: 0\n")

		migration, err := reconcilePersistedNodeAddress(dir, "", nil)
		require.NoError(t, err)
		require.Nil(t, migration)
	})
}

func TestReconcilePersistedNodeAddressRewritesIdentityAndStore(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, infoFileName), "ID: 42\nAddress: 10.244.0.10:9001\nRole: 0\n")
	writeYAML(t, filepath.Join(dir, clusterFileName), ""+
		"- ID: 7\n  Address: 10.244.0.8:9001\n  Role: 0\n"+
		"- ID: 9\n  Address: 10.244.0.9:9001\n  Role: 2\n"+
		"- ID: 42\n  Address: 10.244.0.10:9001\n  Role: 2\n")

	seeds := []string{
		"caesium-0.caesium-headless.default.svc.cluster.local:9001",
		"10.244.0.9:9001", // already known; must not be duplicated
	}

	migration, err := reconcilePersistedNodeAddress(dir, "10.244.0.45:9001", seeds)
	require.NoError(t, err)
	require.NotNil(t, migration)
	require.Equal(t, uint64(42), migration.ID)
	require.Equal(t, "10.244.0.10:9001", migration.OldAddress)
	require.Equal(t, "10.244.0.45:9001", migration.NewAddress)

	info, ok, err := readNodeIdentity(dir)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, uint64(42), info.ID)
	require.Equal(t, "10.244.0.45:9001", info.Address)

	members := readStore(t, dir)
	addresses := make(map[uint64]string, len(members))
	var seedAddrs []string
	for _, member := range members {
		if member.ID == 0 {
			seedAddrs = append(seedAddrs, member.Address)
			continue
		}
		addresses[member.ID] = member.Address
	}
	require.Equal(t, map[uint64]string{
		7:  "10.244.0.8:9001",
		9:  "10.244.0.9:9001",
		42: "10.244.0.45:9001",
	}, addresses)
	require.Equal(t, []string{"caesium-0.caesium-headless.default.svc.cluster.local:9001"}, seedAddrs)

	// Reconciling again is a no-op now that the identity matches.
	again, err := reconcilePersistedNodeAddress(dir, "10.244.0.45:9001", seeds)
	require.NoError(t, err)
	require.Nil(t, again)
}

func TestReconcilePersistedNodeAddressRejectsHalfInitializedDirectory(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, infoFileName), "ID: 42\nAddress: 10.0.0.1:9001\nRole: 0\n")

	_, err := reconcilePersistedNodeAddress(dir, "10.0.0.2:9001", nil)
	require.ErrorContains(t, err, clusterFileName)
}

// startNode boots a bare dqlite app, the way go-dqlite would without any of the
// reconciliation in this package.
func startNode(t *testing.T, ctx context.Context, dir, address string, cluster []string) *dqliteapp.App {
	t.Helper()
	opts := []dqliteapp.Option{dqliteapp.WithAddress(address)}
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

// restartNode reopens a data directory the way cmd/start does, through the
// reconciliation this package installs.
func restartNode(t *testing.T, ctx context.Context, dir, address string, seeds []string) (*dqliteapp.App, error) {
	t.Helper()
	opts := []dqliteapp.Option{dqliteapp.WithAddress(address)}
	if len(seeds) > 0 {
		opts = append(opts, dqliteapp.WithCluster(seeds))
	}
	openCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	return openNativeApp(openCtx, dir, address, seeds, opts...)
}

func clusterMembers(t *testing.T, ctx context.Context, app *dqliteapp.App) map[uint64]client.NodeInfo {
	t.Helper()
	cli, err := app.FindLeader(ctx)
	require.NoError(t, err)
	defer func() { _ = cli.Close() }()
	members, err := cli.Cluster(ctx)
	require.NoError(t, err)

	byID := make(map[uint64]client.NodeInfo, len(members))
	for _, member := range members {
		byID[member.ID] = member
	}
	return byID
}

// TestDqliteRefusesToStartAtANewAddress pins the upstream behaviour this whole
// file exists for: go-dqlite fails hard when a data directory created at one
// address is handed another. That is what crash-looped a replaced StatefulSet
// pod in issue #493.
func TestDqliteRefusesToStartAtANewAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dir := t.TempDir()
	app := startNode(t, ctx, dir, "127.0.0.1:9401", nil)
	require.NoError(t, app.Close())

	_, err := dqliteapp.New(dir, dqliteapp.WithAddress("127.0.0.2:9401"))
	require.ErrorContains(t, err, `address "127.0.0.1:9401" in info.yaml does not match "127.0.0.2:9401"`)
}

// TestSoleMemberRejoinsAtNewAddress covers a single-replica deployment whose
// pod is replaced: there is no leader to route a membership change through, so
// the raft configuration has to be rewritten locally.
func TestSoleMemberRejoinsAtNewAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dir := t.TempDir()
	oldAddr, newAddr := "127.0.0.1:9411", "127.0.0.2:9411"

	app := startNode(t, ctx, dir, oldAddr, nil)
	id := app.ID()
	db, err := app.Open(ctx, "test")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "CREATE TABLE durable (n INT)")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "INSERT INTO durable VALUES (7)")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	require.NoError(t, app.Close())

	app, err = restartNode(t, ctx, dir, newAddr, nil)
	require.NoError(t, err)
	defer func() { _ = app.Close() }()
	require.Equal(t, id, app.ID())

	members := clusterMembers(t, ctx, app)
	require.Len(t, members, 1)
	require.Equal(t, newAddr, members[id].Address)

	db, err = app.Open(ctx, "test")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	var n int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT n FROM durable").Scan(&n))
	require.Equal(t, 7, n)

	// go-dqlite refreshes cluster.yaml from the raft configuration, so a stale
	// self address is not inert: it poisons the discovery cache and the SQL
	// driver above would never find the leader. Leadership is also recognised
	// by node ID rather than by an address that can go stale.
	require.NoFileExists(t, filepath.Join(dir, repairFileName))

	cli, err := app.FindLeader(ctx)
	require.NoError(t, err)
	defer func() { _ = cli.Close() }()
	leader, err := cli.Leader(ctx)
	require.NoError(t, err)
	require.NotNil(t, leader)
	require.Equal(t, id, leader.ID, "leadership must be recognised by node ID")
	require.Equal(t, newAddr, localAddress(app, members[id]))
}

// TestStaleSingletonCacheNeverForcesRecovery is the review finding behind the
// authoritative-membership rule: cluster.yaml is a discovery cache go-dqlite
// refreshes on a 30s timer, so a bootstrap node can still hold a singleton copy
// long after two peers joined and became voters. Replacing that pod must never
// rewrite the raft configuration down to one member — that would fork a live
// cluster — so this test freezes the cache back to a singleton and requires the
// node to rejoin the real three-member cluster instead.
func TestStaleSingletonCacheNeverForcesRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	dirs := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	addrs := []string{"127.0.0.1:9441", "127.0.0.1:9442", "127.0.0.1:9443"}
	apps := make([]*dqliteapp.App, len(dirs))
	for idx := range dirs {
		var seeds []string
		if idx > 0 {
			seeds = addrs[:1]
		}
		apps[idx] = startNode(t, ctx, dirs[idx], addrs[idx], seeds)
	}
	defer func() {
		for _, app := range apps {
			if app != nil {
				_ = app.Close()
			}
		}
	}()

	db, err := apps[1].Open(ctx, "test")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	_, err = db.ExecContext(ctx, "CREATE TABLE durable (n INT)")
	require.NoError(t, err)

	bootstrap := apps[0].ID()
	require.Len(t, clusterMembers(t, ctx, apps[1]), 3)

	// Replace the bootstrap node, and hand it back the singleton discovery
	// cache it held before its peers joined.
	require.NoError(t, apps[0].Close())
	apps[0] = nil

	newAddr := "127.0.0.2:9441"
	writeYAML(t, filepath.Join(dirs[0], clusterFileName),
		fmt.Sprintf("- ID: %d\n  Address: %s\n  Role: 0\n", bootstrap, addrs[0]))

	// Seeds are what the chart gives ordinal 0: nothing.
	apps[0], err = restartNode(t, ctx, dirs[0], newAddr, nil)
	require.NoError(t, err)

	// The cluster must still be three members, not two clusters of one and two.
	for _, app := range apps {
		members := clusterMembers(t, ctx, app)
		require.Len(t, members, 3, "membership must never be forced down to a singleton")
		require.Equal(t, newAddr, members[bootstrap].Address)
		require.Equal(t, client.Voter, members[bootstrap].Role)
	}
}

// TestReplacedMemberRejoinsAtNewAddress is the three-replica pod-replacement
// case from issue #493: one member's data directory survives, its address does
// not, and it has to come back as a voting member the rest of the cluster can
// actually reach.
func TestReplacedMemberRejoinsAtNewAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	dirs := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	addrs := []string{"127.0.0.1:9421", "127.0.0.1:9422", "127.0.0.1:9423"}
	apps := make([]*dqliteapp.App, len(dirs))
	for idx := range dirs {
		var seeds []string
		if idx > 0 {
			seeds = addrs[:1]
		}
		apps[idx] = startNode(t, ctx, dirs[idx], addrs[idx], seeds)
	}
	defer func() {
		for _, app := range apps {
			if app != nil {
				_ = app.Close()
			}
		}
	}()

	db, err := apps[0].Open(ctx, "test")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	_, err = db.ExecContext(ctx, "CREATE TABLE durable (n INT)")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "INSERT INTO durable VALUES (1)")
	require.NoError(t, err)

	replaced := apps[2].ID()
	before := clusterMembers(t, ctx, apps[0])
	require.Len(t, before, 3)
	require.Equal(t, client.Voter, before[replaced].Role)

	// The pod is replaced: same PVC, new IP.
	require.NoError(t, apps[2].Close())
	apps[2] = nil

	newAddr := "127.0.0.2:9423"
	apps[2], err = restartNode(t, ctx, dirs[2], newAddr, addrs[:1])
	require.NoError(t, err)
	require.Equal(t, replaced, apps[2].ID(), "node identity must survive the address change")
	require.NoFileExists(t, filepath.Join(dirs[2], repairFileName))

	after := clusterMembers(t, ctx, apps[0])
	require.Len(t, after, 3, "the replaced member must not be duplicated or dropped")
	require.Equal(t, newAddr, after[replaced].Address)
	require.Equal(t, client.Voter, after[replaced].Role, "the member must get its voting role back")

	// Prior data is still readable from the replaced node.
	replacedDB, err := apps[2].Open(ctx, "test")
	require.NoError(t, err)
	defer func() { _ = replacedDB.Close() }()
	var n int
	require.NoError(t, replacedDB.QueryRowContext(ctx, "SELECT n FROM durable").Scan(&n))
	require.Equal(t, 1, n)

	// Quorum proof: with one of the untouched members stopped, a write can only
	// commit if the leader can actually reach the replaced member at its new
	// address.
	require.NoError(t, apps[1].Close())
	apps[1] = nil
	requireEventualWrite(t, ctx, db, "INSERT INTO durable VALUES (2)")

	var total int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM durable").Scan(&total))
	require.Equal(t, 2, total)
}

// TestInterruptedRepairRestoresVotingRole covers a repair cut short between its
// remove and its re-add — a pod killed mid-migration. Coming back as a
// non-replicating spare while reporting success would leave the cluster a voter
// short, so the next boot has to restore the recorded role before it is done.
func TestInterruptedRepairRestoresVotingRole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	dirs := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	addrs := []string{"127.0.0.1:9431", "127.0.0.1:9432", "127.0.0.1:9433"}
	apps := make([]*dqliteapp.App, len(dirs))
	for idx := range dirs {
		var seeds []string
		if idx > 0 {
			seeds = addrs[:1]
		}
		apps[idx] = startNode(t, ctx, dirs[idx], addrs[idx], seeds)
	}
	defer func() {
		for _, app := range apps {
			if app != nil {
				_ = app.Close()
			}
		}
	}()

	db, err := apps[0].Open(ctx, "test")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	_, err = db.ExecContext(ctx, "CREATE TABLE durable (n INT)")
	require.NoError(t, err)

	orphan := apps[2].ID()
	require.NoError(t, apps[2].Close())
	apps[2] = nil

	// Exactly what a crash after Remove and before Add leaves behind: the
	// journal on disk and no member in the configuration.
	require.NoError(t, writeYAMLFile(dirs[2], repairFileName, addressRepair{
		ID: orphan, Address: addrs[2], Role: client.Voter,
	}))
	cli, err := apps[0].FindLeader(ctx)
	require.NoError(t, err)
	require.NoError(t, cli.Remove(ctx, orphan))
	require.NoError(t, cli.Close())
	require.NotContains(t, clusterMembers(t, ctx, apps[0]), orphan)

	apps[2], err = restartNode(t, ctx, dirs[2], addrs[2], addrs[:1])
	require.NoError(t, err)
	require.NoFileExists(t, filepath.Join(dirs[2], repairFileName),
		"the journal must only clear once the role is confirmed")

	members := clusterMembers(t, ctx, apps[0])
	require.Contains(t, members, orphan)
	require.Equal(t, addrs[2], members[orphan].Address)
	require.Equal(t, client.Voter, members[orphan].Role,
		"an interrupted repair must not leave a non-replicating spare behind")

	// Quorum proof: stopping another voter immediately must still leave a
	// writable cluster, which only holds if the rejoined member really votes.
	require.NoError(t, apps[1].Close())
	apps[1] = nil
	requireEventualWrite(t, ctx, db, "INSERT INTO durable VALUES (1)")
}

// TestSpareMemberAtStaleAddressStillStarts covers the ordering finding: a spare
// whose address changed cannot be promoted, because promotion needs the leader
// to reach it at an address it no longer has. go-dqlite's startup loop retries
// that promotion forever before reporting ready, so the membership repair has to
// run before readiness is awaited or the node never starts at all.
func TestSpareMemberAtStaleAddressStillStarts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	dirs := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	addrs := []string{"127.0.0.1:9451", "127.0.0.1:9452", "127.0.0.1:9453"}
	apps := make([]*dqliteapp.App, len(dirs))
	for idx := range dirs {
		var seeds []string
		if idx > 0 {
			seeds = addrs[:1]
		}
		apps[idx] = startNode(t, ctx, dirs[idx], addrs[idx], seeds)
	}
	defer func() {
		for _, app := range apps {
			if app != nil {
				_ = app.Close()
			}
		}
	}()

	demoted := apps[2].ID()
	cli, err := apps[0].FindLeader(ctx)
	require.NoError(t, err)
	require.NoError(t, cli.Assign(ctx, demoted, client.Spare))
	require.NoError(t, cli.Close())
	require.Equal(t, client.Spare, clusterMembers(t, ctx, apps[0])[demoted].Role)

	require.NoError(t, apps[2].Close())
	apps[2] = nil

	newAddr := "127.0.0.2:9453"
	type restarted struct {
		app *dqliteapp.App
		err error
	}
	done := make(chan restarted, 1)
	go func() {
		app, err := restartNode(t, ctx, dirs[2], newAddr, addrs[:1])
		done <- restarted{app: app, err: err}
	}()

	select {
	case result := <-done:
		require.NoError(t, result.err)
		apps[2] = result.app
	case <-time.After(3 * time.Minute):
		t.Fatal("a spare whose address changed never finished starting")
	}

	members := clusterMembers(t, ctx, apps[0])
	require.Equal(t, newAddr, members[demoted].Address)
}

// requireEventualWrite retries a write while the cluster settles after a member
// goes away; losing a follower can cost one leader election.
func requireEventualWrite(t *testing.T, ctx context.Context, db *sql.DB, stmt string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if _, err = db.ExecContext(ctx, stmt); err == nil {
			return
		}
		time.Sleep(time.Second)
	}
	require.NoError(t, err, fmt.Sprintf("write never committed: %s", stmt))
}

// shortenAddressRepairBudget keeps a test that deliberately cannot reach a
// leader from spending the production retry budget.
func shortenAddressRepairBudget(t *testing.T, budget, interval time.Duration) {
	t.Helper()
	oldBudget, oldInterval := addressRepairBudget, addressRepairInterval
	addressRepairBudget, addressRepairInterval = budget, interval
	t.Cleanup(func() { addressRepairBudget, addressRepairInterval = oldBudget, oldInterval })
}

// TestUnresolvedRepairFailsStartup covers a repair that fails before it ever
// writes a journal. The node is a voter the leader still records at its old
// address, so go-dqlite has nothing to promote and would report ready — and the
// rollout would then replace the next member and take the quorum with it.
// Startup has to fail instead, journal or no journal.
func TestUnresolvedRepairFailsStartup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	shortenAddressRepairBudget(t, 5*time.Second, 100*time.Millisecond)

	dirs := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	addrs := []string{"127.0.0.1:9471", "127.0.0.1:9472", "127.0.0.1:9473"}
	apps := make([]*dqliteapp.App, len(dirs))
	for idx := range dirs {
		var seeds []string
		if idx > 0 {
			seeds = addrs[:1]
		}
		apps[idx] = startNode(t, ctx, dirs[idx], addrs[idx], seeds)
	}
	replaced := apps[2].ID()
	require.Equal(t, client.Voter, clusterMembers(t, ctx, apps[0])[replaced].Role)

	// Take the whole cluster down, so the replacement cannot reach a leader and
	// the repair fails at the first step — before any journal exists.
	for idx := range apps {
		require.NoError(t, apps[idx].Close())
		apps[idx] = nil
	}

	app, err := restartNode(t, ctx, dirs[2], "127.0.0.2:9473", addrs[:1])
	require.Error(t, err, "a node the cluster cannot reach must not report ready")
	require.Nil(t, app)
	require.ErrorContains(t, err, "could not reconcile this node's cluster membership")
	require.NoFileExists(t, filepath.Join(dirs[2], repairFileName),
		"the failure must be enforced without relying on a journal")
}

// TestNeverPromotedSpareRejoinsAtNewAddress covers a member that was added as a
// spare and never promoted. A spare neither replicates the log nor votes, so it
// holds no raft configuration of its own — membership has to come from the
// leader, or its address is never repaired at all.
func TestNeverPromotedSpareRejoinsAtNewAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	dirs := []string{t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()}
	addrs := []string{"127.0.0.1:9481", "127.0.0.1:9482", "127.0.0.1:9483", "127.0.0.1:9484"}
	apps := make([]*dqliteapp.App, len(dirs))

	// Voters 3 / stand-bys 0 leaves a fourth node nothing to be promoted to, so
	// it joins as a spare and stays one.
	start := func(idx int) *dqliteapp.App {
		opts := []dqliteapp.Option{
			dqliteapp.WithAddress(addrs[idx]),
			dqliteapp.WithVoters(3),
			dqliteapp.WithStandBys(0),
		}
		if idx > 0 {
			opts = append(opts, dqliteapp.WithCluster(addrs[:1]))
		}
		app, err := dqliteapp.New(dirs[idx], opts...)
		require.NoError(t, err)
		readyCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		require.NoError(t, app.Ready(readyCtx))
		return app
	}
	for idx := range dirs {
		apps[idx] = start(idx)
	}
	defer func() {
		for _, app := range apps {
			if app != nil {
				_ = app.Close()
			}
		}
	}()

	spare := apps[3].ID()
	require.Equal(t, client.Spare, clusterMembers(t, ctx, apps[0])[spare].Role,
		"the fourth node must never have been promoted")

	// It holds no configuration of its own, which is what makes the leader the
	// only place its membership can be read from.
	_, localErr := localClusterConfiguration(ctx, apps[3])
	require.ErrorIs(t, localErr, ErrNoLocalConfiguration)

	require.NoError(t, apps[3].Close())
	apps[3] = nil

	newAddr := "127.0.0.2:9484"
	var err error
	apps[3], err = restartNode(t, ctx, dirs[3], newAddr, addrs[:1])
	require.NoError(t, err)
	require.Equal(t, spare, apps[3].ID())
	require.NoFileExists(t, filepath.Join(dirs[3], repairFileName))

	members := clusterMembers(t, ctx, apps[0])
	require.Len(t, members, 4)
	require.Equal(t, newAddr, members[spare].Address,
		"a never-promoted spare must still be repaired onto its new address")
	require.Equal(t, client.Spare, members[spare].Role)
}
