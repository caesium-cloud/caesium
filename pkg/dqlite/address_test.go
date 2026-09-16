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
	require.False(t, migration.SoleMember)

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

// startNode boots a dqlite app, waiting for it to be ready.
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

	migration, err := reconcilePersistedNodeAddress(dir, newAddr, nil)
	require.NoError(t, err)
	require.NotNil(t, migration)
	require.True(t, migration.SoleMember)
	require.Equal(t, id, migration.ID)

	app = startNode(t, ctx, dir, newAddr, nil)
	defer func() { _ = app.Close() }()

	require.NoError(t, ensureClusterAddress(ctx, app, 6, time.Second))

	members := clusterMembers(t, ctx, app)
	require.Len(t, members, 1)
	require.Equal(t, newAddr, members[id].Address)

	db, err = app.Open(ctx, "test")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	var n int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT n FROM durable").Scan(&n))
	require.Equal(t, 7, n)

	// The node's own view of the leader must agree with the address it now
	// listens on, otherwise every "am I the owner?" comparison is wrong.
	cli, err := app.FindLeader(ctx)
	require.NoError(t, err)
	defer func() { _ = cli.Close() }()
	leader, err := cli.Leader(ctx)
	require.NoError(t, err)
	require.NotNil(t, leader)
	require.Equal(t, app.Address(), leader.Address)
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
	migration, err := reconcilePersistedNodeAddress(dirs[2], newAddr, addrs[:1])
	require.NoError(t, err)
	require.NotNil(t, migration)
	require.False(t, migration.SoleMember)
	require.Equal(t, replaced, migration.ID)
	require.Equal(t, addrs[2], migration.OldAddress)

	apps[2] = startNode(t, ctx, dirs[2], newAddr, nil)
	require.Equal(t, replaced, apps[2].ID(), "node identity must survive the address change")

	// Before the repair the cluster still dials the address the pod no longer
	// has, so the member is unreachable even though it looks healthy locally.
	require.Equal(t, addrs[2], clusterMembers(t, ctx, apps[0])[replaced].Address)

	require.NoError(t, ensureClusterAddress(ctx, apps[2], 6, 2*time.Second))

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

// TestInterruptedRepairRejoins covers a repair that was cut short between its
// remove and its re-add — a pod killed mid-migration. Nothing is left on disk to
// notice it by, so the next boot has to see that this node is simply not in the
// configuration and rejoin.
func TestInterruptedRepairRejoins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
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
			_ = app.Close()
		}
	}()

	orphan := apps[2].ID()

	cli, err := apps[0].FindLeader(ctx)
	require.NoError(t, err)
	require.NoError(t, cli.Remove(ctx, orphan))
	require.NoError(t, cli.Close())
	require.NotContains(t, clusterMembers(t, ctx, apps[0]), orphan)

	require.NoError(t, ensureClusterAddress(ctx, apps[2], 6, 2*time.Second))

	members := clusterMembers(t, ctx, apps[0])
	require.Contains(t, members, orphan)
	require.Equal(t, addrs[2], members[orphan].Address)
}

// requireEventualWrite retries a write while the cluster settles after a member
// goes away; losing a follower can cost one leader election.
func requireEventualWrite(t *testing.T, ctx context.Context, db *sql.DB, stmt string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if _, err = db.ExecContext(ctx, stmt); err == nil {
			return
		}
		time.Sleep(time.Second)
	}
	require.NoError(t, err, fmt.Sprintf("write never committed: %s", stmt))
}
