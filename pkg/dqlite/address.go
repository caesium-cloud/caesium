package dqlite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/caesium-cloud/caesium/pkg/log"
	godqlite "github.com/canonical/go-dqlite/v3"
	dqliteapp "github.com/canonical/go-dqlite/v3/app"
	"github.com/canonical/go-dqlite/v3/client"
	"gopkg.in/yaml.v3"
)

const (
	// go-dqlite's app package persists this node's own raft identity in
	// info.yaml and its last known view of the cluster in cluster.yaml, both
	// inside the data directory. Neither constant is exported by go-dqlite, so
	// the names are repeated here.
	infoFileName    = "info.yaml"
	clusterFileName = "cluster.yaml"

	// How long to keep trying to correct this node's address in the raft
	// configuration once the local node is up. A rolling pod replacement
	// normally succeeds on the first attempt; the retries cover a leader
	// election in flight.
	addressRepairAttempts = 12
	addressRepairInterval = 5 * time.Second
)

// ErrAddressRepairWhileLeading is returned when this node's address is stale in
// the raft configuration but this node is currently the leader, so it cannot
// remove and re-add itself. The caller retries; a node the rest of the cluster
// cannot reach does not stay leader for long.
var ErrAddressRepairWhileLeading = errors.New("dqlite: cannot rewrite own address while holding leadership")

// AddressMigration records a node whose persisted dqlite address no longer
// matches the address it has been configured with.
//
// dqlite writes the node's advertised address into info.yaml on first boot and
// refuses to start when a later boot supplies a different one. Kubernetes
// guarantees a StatefulSet pod a stable *identity* and a stable PVC, never a
// stable pod IP, so any pod replacement can hand the same data directory a new
// address (see issue #493). dqlite's bind address must be a numeric IP —
// hostnames are rejected by dqlite_node_set_bind_address — so the address
// cannot simply be pinned to the headless-service DNS name; it has to be
// reconciled instead.
type AddressMigration struct {
	// ID is the dqlite node ID recorded in info.yaml. It is stable across the
	// address change and is what identifies this member to the cluster.
	ID uint64
	// OldAddress is the address recorded in info.yaml before the migration.
	OldAddress string
	// NewAddress is the configured address the node now listens on.
	NewAddress string
	// SoleMember is true when cluster.yaml lists no member other than this
	// node, so the raft configuration was rewritten locally rather than
	// through a leader.
	SoleMember bool
}

// readNodeIdentity reads go-dqlite's info.yaml from dir. The boolean reports
// whether the file exists; a data directory that has never been initialized has
// no identity to reconcile.
func readNodeIdentity(dir string) (client.NodeInfo, bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, infoFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return client.NodeInfo{}, false, nil
		}
		return client.NodeInfo{}, false, fmt.Errorf("read %s: %w", infoFileName, err)
	}

	var info client.NodeInfo
	if err := yaml.Unmarshal(data, &info); err != nil {
		return client.NodeInfo{}, false, fmt.Errorf("parse %s: %w", infoFileName, err)
	}
	return info, true, nil
}

// writeNodeIdentity replaces info.yaml atomically, so a crash mid-write cannot
// leave the data directory with an unparseable identity.
func writeNodeIdentity(dir string, info client.NodeInfo) error {
	data, err := yaml.Marshal(info)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", infoFileName, err)
	}

	tmp, err := os.CreateTemp(dir, infoFileName+".*")
	if err != nil {
		return fmt.Errorf("create temporary %s: %w", infoFileName, err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", infoFileName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync %s: %w", infoFileName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", infoFileName, err)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", infoFileName, err)
	}
	if err := os.Rename(name, filepath.Join(dir, infoFileName)); err != nil {
		return fmt.Errorf("replace %s: %w", infoFileName, err)
	}
	return nil
}

// reconcilePersistedNodeAddress makes a data directory that was initialized at
// a different address usable at the configured one.
//
// It rewrites the node's own entry in cluster.yaml (the client node store used
// to find a leader) and then info.yaml itself, so go-dqlite's app.New stops
// refusing to start. When this node is the only member, the raft configuration
// is also rewritten locally — there is no leader to go through, and nothing to
// diverge from. In every other case the raft configuration is corrected after
// startup by repairClusterAddress, which needs the node running to reach the
// leader.
//
// seeds are the configured bootstrap peer addresses (CAESIUM_DATABASE_NODES).
// They are merged into the node store as extra leader-discovery candidates,
// because the peer addresses cluster.yaml already holds may themselves be stale
// when several pods were replaced. go-dqlite overwrites the store with the
// leader's view on its first refresh, so the merge only matters during startup.
//
// It returns nil when there is nothing to do: no data directory yet, or an
// address that already matches.
func reconcilePersistedNodeAddress(dir, address string, seeds []string) (*AddressMigration, error) {
	if address == "" {
		return nil, nil
	}

	info, ok, err := readNodeIdentity(dir)
	if err != nil {
		return nil, err
	}
	if !ok || info.Address == address {
		return nil, nil
	}

	storePath := filepath.Join(dir, clusterFileName)
	if _, err := os.Stat(storePath); err != nil {
		// go-dqlite rejects a data directory holding one file and not the
		// other; leave that diagnosis to app.New rather than creating the
		// missing file here.
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf(
				"dqlite data directory %s has %s but no %s", dir, infoFileName, clusterFileName)
		}
		return nil, fmt.Errorf("stat %s: %w", clusterFileName, err)
	}

	store, err := client.NewYamlNodeStore(storePath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", clusterFileName, err)
	}

	ctx := context.Background()
	members, err := store.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", clusterFileName, err)
	}

	migration := &AddressMigration{
		ID:         info.ID,
		OldAddress: info.Address,
		NewAddress: address,
	}

	updated := make([]client.NodeInfo, 0, len(members)+len(seeds))
	peers := 0
	for _, member := range members {
		if member.ID == info.ID || member.Address == info.Address {
			member.ID = info.ID
			member.Address = address
		} else {
			peers++
		}
		updated = append(updated, member)
	}
	migration.SoleMember = peers == 0

	known := make(map[string]struct{}, len(updated))
	for _, member := range updated {
		known[member.Address] = struct{}{}
	}
	if !migration.SoleMember {
		for _, seed := range seeds {
			if seed == "" || seed == address {
				continue
			}
			if _, seen := known[seed]; seen {
				continue
			}
			known[seed] = struct{}{}
			updated = append(updated, client.NodeInfo{Address: seed})
		}
	}

	if err := store.Set(ctx, updated); err != nil {
		return nil, fmt.Errorf("rewrite %s: %w", clusterFileName, err)
	}

	if migration.SoleMember {
		// A single-member cluster has no peer that could hold a more recent
		// log, so forcing the raft configuration here is the documented
		// recovery path and not a divergence risk. Without it the node keeps
		// reporting its old address as the leader's, which breaks every
		// "am I the leader?" comparison against the configured address.
		recovered := []client.NodeInfo{{ID: info.ID, Address: address, Role: client.Voter}}
		if err := godqlite.ReconfigureMembershipExt(dir, recovered); err != nil {
			return nil, fmt.Errorf("rewrite raft configuration for sole member: %w", err)
		}
	}

	info.Address = address
	if err := writeNodeIdentity(dir, info); err != nil {
		return nil, err
	}

	return migration, nil
}

// repairClusterAddress corrects the raft configuration when it still lists this
// node at an address it no longer listens on.
//
// The address in the configuration is what every other member dials, so a node
// whose entry is stale is unreachable by the leader even though it can reach
// the leader itself — it looks healthy locally while contributing nothing to
// quorum. dqlite has no "update address" operation (raft_add rejects a
// duplicate ID), so the member is removed and re-added at its new address under
// the same node ID, then restored to the role it held.
//
// It reports whether it changed anything.
func repairClusterAddress(ctx context.Context, app *dqliteapp.App) (bool, error) {
	cli, err := app.FindLeader(ctx)
	if err != nil {
		return false, fmt.Errorf("find leader: %w", err)
	}
	defer func() { _ = cli.Close() }()

	members, err := cli.Cluster(ctx)
	if err != nil {
		return false, fmt.Errorf("read cluster membership: %w", err)
	}

	var current *client.NodeInfo
	for idx := range members {
		if members[idx].ID == app.ID() {
			current = &members[idx]
			break
		}
	}
	if current != nil && current.Address == app.Address() {
		return false, nil
	}

	if current == nil {
		// This node holds a data directory for the cluster but is not in the
		// configuration, which is what a repair interrupted between its remove
		// and its re-add leaves behind. Nothing else in Caesium removes a
		// member, so finish the job rather than run on as a non-member.
		log.Warn(
			"this node is absent from the dqlite cluster configuration; rejoining",
			"node_id", app.ID(), "node_address", app.Address())
		if err := cli.Add(ctx, client.NodeInfo{ID: app.ID(), Address: app.Address(), Role: client.Spare}); err != nil {
			return false, fmt.Errorf("rejoin as member %d at %s: %w", app.ID(), app.Address(), err)
		}
		return true, nil
	}
	stale := current

	if len(members) < 2 {
		// Removing the only member would destroy the cluster. A sole member's
		// configuration is rewritten offline by reconcilePersistedNodeAddress
		// instead, so reaching here means that did not happen.
		return false, fmt.Errorf(
			"dqlite: node %d is the only member and still recorded at %s", app.ID(), stale.Address)
	}

	leader, err := cli.Leader(ctx)
	if err != nil {
		return false, fmt.Errorf("read leader: %w", err)
	}
	if leader != nil && leader.ID == app.ID() {
		return false, ErrAddressRepairWhileLeading
	}

	role := stale.Role
	if err := cli.Remove(ctx, app.ID()); err != nil {
		return false, fmt.Errorf("remove stale member %d: %w", app.ID(), err)
	}
	if err := cli.Add(ctx, client.NodeInfo{ID: app.ID(), Address: app.Address(), Role: client.Spare}); err != nil {
		return false, fmt.Errorf("re-add member %d at %s: %w", app.ID(), app.Address(), err)
	}
	if role != client.Spare {
		if err := cli.Assign(ctx, app.ID(), role); err != nil {
			// The member is back in the cluster at the right address;
			// go-dqlite's role adjustment loop promotes it on its own if this
			// fails, so this is not fatal.
			return true, fmt.Errorf("restore role %s for member %d: %w", role, app.ID(), err)
		}
	}
	return true, nil
}

// ensureClusterAddress keeps retrying repairClusterAddress until the raft
// configuration agrees with the address this node listens on.
//
// It runs on every boot rather than only after a detected info.yaml migration:
// a crash between rewriting info.yaml and correcting the membership would
// otherwise leave the member permanently unreachable, with nothing left on disk
// to notice it by.
func ensureClusterAddress(ctx context.Context, app *dqliteapp.App, attempts int, interval time.Duration) error {
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
			}
		}

		repaired, err := repairClusterAddress(ctx, app)
		if repaired {
			log.Info(
				"dqlite cluster membership updated to this node's current address",
				"node_id", app.ID(), "node_address", app.Address())
		}
		if err == nil {
			return nil
		}
		lastErr = err
		log.Warn(
			"could not reconcile this node's dqlite cluster address yet",
			"node_id", app.ID(), "node_address", app.Address(),
			"attempt", attempt+1, "attempts", attempts, "error", err)
	}
	return lastErr
}
