package dqlite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/pkg/log"
	godqlite "github.com/canonical/go-dqlite/v3"
	dqliteapp "github.com/canonical/go-dqlite/v3/app"
	"github.com/canonical/go-dqlite/v3/client"
	"gopkg.in/yaml.v3"
)

const (
	// go-dqlite's app package persists this node's own raft identity in
	// info.yaml, its last known view of the cluster in cluster.yaml, and a
	// marker file while a brand new node still has to join. None of the names
	// are exported by go-dqlite, so they are repeated here.
	infoFileName    = "info.yaml"
	clusterFileName = "cluster.yaml"
	joinFileName    = "join"

	// repairFileName is Caesium's own journal, described on addressRepair.
	repairFileName = "address-repair.yaml"

	// How long to wait for the local node to answer a request for its own raft
	// configuration after it has been started.
	localConfigurationTimeout = 30 * time.Second
	localConfigurationRetry   = time.Second

	// How long to wait when checking whether some other address answers as a
	// live dqlite node. Only used to refuse a local raft reconfiguration.
	peerProbeTimeout = 3 * time.Second

	// How long a single attempt may spend looking for a leader. go-dqlite's
	// connector walks every candidate with its own backoff, which on a cluster
	// that is entirely down runs far longer than one repair attempt should.
	leaderConnectTimeout = 15 * time.Second
)

// How long to keep trying to correct this node's address in the raft
// configuration, and how long to wait between attempts.
//
// A rolling pod replacement normally succeeds on the first attempt. The budget
// covers a leader election in flight, and the shorter contention interval
// covers a leader that is busy with a configuration change of its own — which
// is the steady state when it is retrying a promotion of this very node, since
// that promotion cannot complete until this repair lands. Variables rather than
// constants only so tests can shorten the budget.
var (
	addressRepairBudget            = 90 * time.Second
	addressRepairInterval          = 5 * time.Second
	addressRepairContentionRetry   = 500 * time.Millisecond
	errConfigurationChangeInFlight = "a configuration change is already in progress"
)

// ErrAddressRepairWhileLeading is returned when this node's address is stale in
// the raft configuration but this node is currently the leader, so it cannot
// remove and re-add itself. If a reachable voter can take leadership, the
// caller transfers to it and retries; otherwise startup eventually fails.
var ErrAddressRepairWhileLeading = errors.New("dqlite: cannot rewrite own address while holding leadership")

var errLeadershipTransferred = errors.New("dqlite: transferred leadership before address repair")

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
}

// addressRepair is the journal Caesium writes before it mutates cluster
// membership on its own behalf.
//
// dqlite has no "update address" operation — raft_add rejects a duplicate ID —
// so moving a member's address means removing it and adding it back. That is
// two round trips, and a pod killed between them leaves a cluster one voter
// short with nothing on disk to notice it by. The journal records the identity
// and the role that has to be restored, and is only cleared once the leader
// reports this node back at the right address *and* in the right role.
type addressRepair struct {
	ID      uint64          `yaml:"ID"`
	Address string          `yaml:"Address"`
	Role    client.NodeRole `yaml:"Role"`
}

// readNodeIdentity reads go-dqlite's info.yaml from dir. The boolean reports
// whether the file exists; a data directory that has never been initialized has
// no identity to reconcile.
func readNodeIdentity(dir string) (client.NodeInfo, bool, error) {
	var info client.NodeInfo
	ok, err := readYAMLFile(dir, infoFileName, &info)
	return info, ok, err
}

// readRepairJournal reads Caesium's in-flight membership repair, if any.
func readRepairJournal(dir string) (addressRepair, bool, error) {
	var repair addressRepair
	ok, err := readYAMLFile(dir, repairFileName, &repair)
	return repair, ok, err
}

func readYAMLFile(dir, name string, into any) (bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w", name, err)
	}
	if err := yaml.Unmarshal(data, into); err != nil {
		return false, fmt.Errorf("parse %s: %w", name, err)
	}
	return true, nil
}

// writeYAMLFile replaces a file atomically and flushes it, so a crash mid-write
// can neither leave an unparseable file behind nor lose a journal that a
// membership change is about to depend on.
func writeYAMLFile(dir, name string, from any) error {
	data, err := yaml.Marshal(from)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", name, err)
	}

	tmp, err := os.CreateTemp(dir, name+".*")
	if err != nil {
		return fmt.Errorf("create temporary %s: %w", name, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", name, err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("replace %s: %w", name, err)
	}
	return nil
}

func clearRepairJournal(dir string) error {
	if err := os.Remove(filepath.Join(dir, repairFileName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", repairFileName, err)
	}
	return nil
}

func fileExists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// reconcilePersistedNodeAddress makes a data directory that was initialized at
// a different address usable at the configured one.
//
// It rewrites the node's own entry in cluster.yaml (the client node store used
// to find a leader) and then info.yaml itself, so go-dqlite's app.New stops
// refusing to start. It deliberately does NOT touch the raft configuration:
// cluster.yaml is a periodically refreshed *discovery cache*, not authoritative
// membership, so nothing here may be used to decide how many members the
// cluster has. That decision is taken later, from the started node's own
// committed raft configuration.
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

	updated := make([]client.NodeInfo, 0, len(members)+len(seeds))
	known := make(map[string]struct{}, len(members)+len(seeds))
	for _, member := range members {
		if member.ID == info.ID || member.Address == info.Address {
			member.ID = info.ID
			member.Address = address
		}
		if _, seen := known[member.Address]; seen {
			continue
		}
		known[member.Address] = struct{}{}
		updated = append(updated, member)
	}
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

	if err := store.Set(ctx, updated); err != nil {
		return nil, fmt.Errorf("rewrite %s: %w", clusterFileName, err)
	}

	previous := info.Address
	info.Address = address
	if err := writeYAMLFile(dir, infoFileName, info); err != nil {
		return nil, err
	}

	return &AddressMigration{ID: info.ID, OldAddress: previous, NewAddress: address}, nil
}

// ErrNoLocalConfiguration means the local node has no committed raft
// configuration of its own to read.
//
// It is the normal state of a node that was added as a spare and never
// promoted: a spare neither replicates the log nor participates in quorum, so
// it holds no configuration even though the leader has it registered. Such a
// node still needs its address repaired, just not from a local proof.
var ErrNoLocalConfiguration = errors.New("dqlite: local node has no raft configuration of its own")

// localClusterConfiguration asks the started local node for its own committed
// raft configuration.
//
// This is the authoritative membership for this node: unlike cluster.yaml it is
// replicated state, not a cache that go-dqlite refreshes on a timer (30s by
// default), so a bootstrap node whose cache still says "one member" long after
// two peers joined cannot mislead it. Only that guarantee makes it safe to
// decide that a cluster has a single member.
//
// An empty answer is definitive rather than transient, and returns
// ErrNoLocalConfiguration immediately.
func localClusterConfiguration(ctx context.Context, app *dqliteapp.App) ([]client.NodeInfo, error) {
	deadline := time.Now().Add(localConfigurationTimeout)
	var lastErr error
	for {
		members, err := func() ([]client.NodeInfo, error) {
			cli, err := app.Client(ctx)
			if err != nil {
				return nil, err
			}
			defer func() { _ = cli.Close() }()
			return cli.Cluster(ctx)
		}()
		switch {
		case err == nil && len(members) > 0:
			return members, nil
		case err == nil:
			return nil, ErrNoLocalConfiguration
		default:
			lastErr = err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("read local raft configuration: %w", lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(localConfigurationRetry):
		}
	}
}

// leaderClusterConfiguration reads membership from the cluster leader, for the
// nodes that hold no configuration of their own.
//
// The leader's view is authoritative about who is registered and at what
// address; it is only insufficient for the one decision that must never be
// taken on hearsay — that this node is a cluster's sole member, which is what
// authorizes rewriting the raft configuration locally.
func leaderClusterConfiguration(ctx context.Context, candidates []string) ([]client.NodeInfo, error) {
	cli, err := findLeaderAmong(ctx, candidates)
	if err != nil {
		return nil, fmt.Errorf("find leader: %w", err)
	}
	defer func() { _ = cli.Close() }()
	return cli.Cluster(ctx)
}

func memberByID(members []client.NodeInfo, id uint64) *client.NodeInfo {
	for idx := range members {
		if members[idx].ID == id {
			return &members[idx]
		}
	}
	return nil
}

// peerAddresses returns every address in the configuration other than this
// node's own, merged with the extra candidates (the discovery cache and the
// configured seeds) so leader discovery still has somewhere to start when the
// configuration itself is what went stale.
func peerAddresses(members []client.NodeInfo, id uint64, self string, extra ...[]string) []string {
	seen := map[string]struct{}{self: {}}
	addresses := make([]string, 0, len(members))
	add := func(address string) {
		if address == "" {
			return
		}
		if _, ok := seen[address]; ok {
			return
		}
		seen[address] = struct{}{}
		addresses = append(addresses, address)
	}
	for _, member := range members {
		if member.ID == id {
			continue
		}
		add(member.Address)
	}
	for _, group := range extra {
		for _, address := range group {
			add(address)
		}
	}
	return addresses
}

// anyNodeReachable reports whether any of the given addresses answers as a live
// dqlite node. It exists to refuse a local raft reconfiguration whenever
// something else is still out there, however this node's view of membership got
// to look like a singleton.
func anyNodeReachable(ctx context.Context, addresses []string) (string, bool) {
	for _, address := range addresses {
		probeCtx, cancel := context.WithTimeout(ctx, peerProbeTimeout)
		cli, err := client.New(probeCtx, address)
		cancel()
		if err != nil {
			continue
		}
		_ = cli.Close()
		return address, true
	}
	return "", false
}

// recoverSoleMemberAddress rewrites the raft configuration of a cluster whose
// only member is this node.
//
// It has to be done. go-dqlite refreshes cluster.yaml from the raft
// configuration on a timer, so a stale self address does not stay inert: it
// overwrites the discovery cache, and the SQL driver then dials an address
// nothing listens on and never finds the leader. A single-member cluster has no
// leader to route a membership change through, so forcing the configuration is
// the documented recovery path — and with no peer to diverge from, it is safe.
//
// Safe only under both of these, which the caller must establish: membership
// came from the node's own committed raft configuration and named exactly one
// member — never from the cluster.yaml discovery cache, which lags by up to
// go-dqlite's roles-adjustment interval and can still read as a singleton long
// after peers joined — and no other address this node knows of answers as a
// live node. The node must be stopped; raft_recover refuses to run against a
// live one.
func recoverSoleMemberAddress(ctx context.Context, dir string, id uint64, address string, candidates []string) error {
	if reachable, ok := anyNodeReachable(ctx, candidates); ok {
		return fmt.Errorf(
			"dqlite: refusing to rewrite the raft configuration of node %d: %s answered as a live node",
			id, reachable)
	}
	recovered := []client.NodeInfo{{ID: id, Address: address, Role: client.Voter}}
	if err := godqlite.ReconfigureMembershipExt(dir, recovered); err != nil {
		return err
	}

	// The node that just stopped refreshed cluster.yaml from the configuration
	// it had, so the discovery cache is holding the address that was just
	// recovered away. Left there, the next start dials an address nothing
	// listens on and never finds its own leader.
	store, err := client.NewYamlNodeStore(filepath.Join(dir, clusterFileName))
	if err != nil {
		return fmt.Errorf("open %s: %w", clusterFileName, err)
	}
	if err := store.Set(ctx, recovered); err != nil {
		return fmt.Errorf("rewrite %s: %w", clusterFileName, err)
	}
	return nil
}

// findLeaderAmong connects to the cluster leader, starting from the given
// candidate addresses.
func findLeaderAmong(ctx context.Context, addresses []string) (*client.Client, error) {
	if len(addresses) == 0 {
		return nil, errors.New("no candidate addresses to find a leader through")
	}
	store := client.NewInmemNodeStore()
	nodes := make([]client.NodeInfo, 0, len(addresses))
	for _, address := range addresses {
		nodes = append(nodes, client.NodeInfo{Address: address})
	}
	if err := store.Set(ctx, nodes); err != nil {
		return nil, err
	}
	// Bound the search, not the client: the timeout covers dialling and the
	// handshake, while the caller drives the returned connection on its own
	// context.
	connectCtx, cancel := context.WithTimeout(ctx, leaderConnectTimeout)
	defer cancel()
	return client.FindLeader(connectCtx, store)
}

// repairClusterAddress makes the cluster's record of this node agree with the
// address it actually listens on and the role it is supposed to hold.
//
// The address in the raft configuration is what every other member dials, so a
// node whose entry is stale is unreachable by the leader even though it can
// reach the leader itself — it looks healthy locally while contributing nothing
// to quorum. dqlite has no "update address" operation, so the member is removed
// and re-added under the same node ID and role. A journal on disk carries the
// role across a crash between those two steps, and is only cleared once the
// leader confirms both the address and the role.
//
// It reports whether it changed anything.
func repairClusterAddress(ctx context.Context, dir string, id uint64, address string, candidates []string) (bool, error) {
	journal, journalled, err := readRepairJournal(dir)
	if err != nil {
		return false, err
	}
	// A journal for another identity cannot describe this node. An address
	// change after an interrupted remove does not invalidate the saved role:
	// the next boot must re-add this same node at its latest address in that
	// role, rather than defaulting an absent spare or standby to a voter.
	if journalled && journal.ID != id {
		journalled = false
	}

	cli, err := findLeaderAmong(ctx, candidates)
	if err != nil {
		return false, fmt.Errorf("find leader: %w", err)
	}
	defer func() { _ = cli.Close() }()

	members, err := cli.Cluster(ctx)
	if err != nil {
		return false, fmt.Errorf("read cluster membership: %w", err)
	}

	current := memberByID(members, id)
	role := client.Voter
	switch {
	case journalled:
		role = journal.Role
	case current != nil:
		role = current.Role
	}

	if current != nil && current.Address == address && current.Role == role {
		return false, clearRepairJournal(dir)
	}

	if current != nil && current.Address == address {
		// A previous repair re-added this node but never restored its role, so
		// it is sitting in the cluster without replicating.
		log.Warn("restoring this node's dqlite role after an interrupted repair",
			"node_id", id, "node_address", address, "role", role.String(), "current_role", current.Role.String())
		if err := cli.Assign(ctx, id, role); err != nil {
			return false, fmt.Errorf("restore role %s for member %d: %w", role, id, err)
		}
		return true, confirmMembership(ctx, cli, dir, id, address, role)
	}

	if current == nil {
		// This node holds a data directory for the cluster but is not in the
		// configuration, which is what a repair interrupted between its remove
		// and its re-add leaves behind. Nothing else in Caesium removes a
		// member, so finish the job rather than run on as a non-member.
		log.Warn("this node is absent from the dqlite cluster configuration; rejoining",
			"node_id", id, "node_address", address, "role", role.String())
		if err := writeYAMLFile(dir, repairFileName, addressRepair{ID: id, Address: address, Role: role}); err != nil {
			return false, err
		}
		if err := cli.Add(ctx, client.NodeInfo{ID: id, Address: address, Role: role}); err != nil {
			return false, fmt.Errorf("rejoin as member %d at %s: %w", id, address, err)
		}
		return true, confirmMembership(ctx, cli, dir, id, address, role)
	}

	if len(members) < 2 {
		// Removing the only member would destroy the cluster. A sole member's
		// configuration is rewritten locally instead, so reaching here means
		// that did not happen.
		return false, fmt.Errorf(
			"dqlite: node %d is the only member and still recorded at %s", id, current.Address)
	}

	leader, err := cli.Leader(ctx)
	if err != nil {
		return false, fmt.Errorf("read leader: %w", err)
	}
	if leader != nil && leader.ID == id {
		return false, transferLeadershipFromStaleNode(ctx, cli, members, id)
	}

	// Journal before the first of the two round trips, so a crash in between
	// leaves the role to restore on disk.
	if err := writeYAMLFile(dir, repairFileName, addressRepair{ID: id, Address: address, Role: role}); err != nil {
		return false, err
	}
	if err := cli.Remove(ctx, id); err != nil {
		return false, fmt.Errorf("remove stale member %d: %w", id, err)
	}
	if err := cli.Add(ctx, client.NodeInfo{ID: id, Address: address, Role: role}); err != nil {
		return false, fmt.Errorf("re-add member %d at %s: %w", id, address, err)
	}
	return true, confirmMembership(ctx, cli, dir, id, address, role)
}

// A replaced voter can win an election through outbound connections even
// though peers still dial its old address. Its heartbeats can maintain that
// leadership indefinitely, so waiting for an election is not enough. Transfer
// to a reachable voter, then reconnect on the next repair attempt; the old
// leader connection must not be used for membership changes after transfer.
func transferLeadershipFromStaleNode(ctx context.Context, cli *client.Client, members []client.NodeInfo, id uint64) error {
	var transferErr error
	for _, member := range members {
		if err := ctx.Err(); err != nil {
			return err
		}
		if member.ID == id || member.Role != client.Voter {
			continue
		}
		if _, reachable := anyNodeReachable(ctx, []string{member.Address}); !reachable {
			continue
		}
		transferCtx, cancel := context.WithTimeout(ctx, leaderConnectTimeout)
		err := cli.Transfer(transferCtx, member.ID)
		cancel()
		if err == nil {
			return fmt.Errorf("%w: voter %d at %s", errLeadershipTransferred, member.ID, member.Address)
		}
		transferErr = errors.Join(transferErr, fmt.Errorf("voter %d at %s: %w", member.ID, member.Address, err))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if transferErr != nil {
		return fmt.Errorf("%w: %v", ErrAddressRepairWhileLeading, transferErr)
	}
	return fmt.Errorf("%w: no other voting member is reachable", ErrAddressRepairWhileLeading)
}

// confirmMembership re-reads the leader's view and only clears the journal once
// this node is recorded at the right address in the right role. Anything else
// leaves the journal in place so the next attempt — or the next boot — finishes
// the repair rather than reporting a reduced quorum as success.
func confirmMembership(ctx context.Context, cli *client.Client, dir string, id uint64, address string, role client.NodeRole) error {
	members, err := cli.Cluster(ctx)
	if err != nil {
		return fmt.Errorf("confirm cluster membership: %w", err)
	}
	current := memberByID(members, id)
	if current == nil {
		return fmt.Errorf("dqlite: member %d is still absent from the cluster after repair", id)
	}
	if current.Address != address {
		return fmt.Errorf(
			"dqlite: member %d is still recorded at %s, not %s", id, current.Address, address)
	}
	if current.Role != role {
		return fmt.Errorf(
			"dqlite: member %d is a %s, not the %s it must be restored to", id, current.Role, role)
	}
	return clearRepairJournal(dir)
}

// ensureClusterAddress keeps retrying repairClusterAddress until the cluster
// agrees with the address this node listens on and the role it must hold.
func ensureClusterAddress(
	ctx context.Context,
	dir string,
	id uint64,
	address string,
	candidates []string,
	budget time.Duration,
) error {
	repairCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var lastErr error
	for attempt := 1; ; attempt++ {
		repaired, err := repairClusterAddress(repairCtx, dir, id, address, candidates)
		if err == nil {
			if repaired {
				log.Info("dqlite cluster membership updated to this node's current address",
					"node_id", id, "node_address", address)
			}
			return nil
		}
		lastErr = err
		log.Warn("could not reconcile this node's dqlite cluster address yet",
			"node_id", id, "node_address", address, "attempt", attempt, "error", err)

		if repairCtx.Err() != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return lastErr
		}
		// The leader refuses overlapping configuration changes. When this node
		// is a spare, the change in its way is the leader's own retrying
		// promotion of this node — which can only ever succeed once this repair
		// has landed — so come back quickly enough to slot between its attempts
		// instead of waiting out the full interval.
		delay := addressRepairInterval
		if strings.Contains(err.Error(), errConfigurationChangeInFlight) || errors.Is(err, errLeadershipTransferred) {
			delay = addressRepairContentionRetry
		}
		select {
		case <-repairCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return lastErr
		case <-time.After(delay):
		}
	}
}

// openNativeApp starts the local dqlite node at the configured address,
// reconciling a data directory that was created at a different one.
//
// Order matters. The membership repair runs *before* App.Ready: go-dqlite's
// startup loop tries to promote this node before it reports ready, and a
// promotion can only complete once the leader can dial the address this node
// actually listens on. Waiting for readiness first would deadlock exactly the
// case this function exists to repair.
func openNativeApp(ctx context.Context, dir, address string, seeds []string, opts ...dqliteapp.Option) (*dqliteapp.App, error) {
	migration, err := reconcilePersistedNodeAddress(dir, address, seeds)
	if err != nil {
		return nil, err
	}
	if migration != nil {
		log.Warn("dqlite node address changed since this data directory was created; migrating",
			"node_id", migration.ID,
			"previous_address", migration.OldAddress,
			"node_address", migration.NewAddress)
	}

	// app.New clears the join marker once a brand new node has joined, so read
	// it before starting: a node that has never been a member has no membership
	// to repair, and must not race its own join.
	joining := fileExists(dir, joinFileName)

	app, err := dqliteapp.New(dir, opts...)
	if err != nil {
		return nil, err
	}
	if joining {
		return readyOrClose(ctx, app)
	}

	cached, seedCandidates := discoveryCandidates(dir, seeds)
	candidates := peerAddresses(nil, app.ID(), app.Address(), cached, seedCandidates)

	// localProof records that membership came from this node's own committed
	// raft configuration. Only that authorizes the local reconfiguration below;
	// everything else is equally well served by the leader's view.
	localProof := true
	members, err := localClusterConfiguration(ctx, app)
	if err != nil {
		localProof = false
		log.Warn("this node has no dqlite raft configuration of its own; "+
			"reading membership from the leader instead",
			"node_id", app.ID(), "node_address", app.Address(), "error", err)
		if members, err = leaderClusterConfiguration(ctx, candidates); err != nil {
			// A never-promoted spare has no local configuration, but the leader
			// may still record it at a stale address. Without either authoritative
			// view, readiness cannot prove that this node is a cluster member at
			// its current address. A later rollout could then replace a voter
			// while this node is unable to take its place.
			_ = app.Close()
			return nil, fmt.Errorf("dqlite: could not read cluster membership from the local node or the leader: %w", err)
		}
	}

	self := memberByID(members, app.ID())
	journalled := fileExists(dir, repairFileName)
	if self != nil && self.Address == app.Address() && !journalled {
		// Nothing to reconcile; skip the leader round trip entirely so an
		// ordinary boot costs nothing.
		return readyOrClose(ctx, app)
	}

	if localProof && len(members) == 1 && self != nil {
		id, listening, previous := app.ID(), app.Address(), self.Address
		// raft_recover refuses to run against a live node, so stop first and
		// start again afterwards.
		if err := app.Close(); err != nil {
			return nil, err
		}
		if err := recoverSoleMemberAddress(
			ctx, dir, id, listening, peerAddresses(members, id, listening, cached, seedCandidates),
		); err != nil {
			return nil, err
		}
		log.Warn("rewrote the raft configuration of a single-member dqlite cluster onto its new address",
			"node_id", id, "previous_address", previous, "node_address", listening)
		if app, err = dqliteapp.New(dir, opts...); err != nil {
			return nil, err
		}
		return readyOrClose(ctx, app)
	}

	if err := ensureClusterAddress(
		ctx, dir, app.ID(), app.Address(),
		peerAddresses(members, app.ID(), app.Address(), cached, seedCandidates),
		addressRepairBudget,
	); err != nil {
		// Reaching here means the cluster does not record this node where it
		// actually listens, whether or not this process got as far as writing a
		// journal — a repair that failed before the journal existed leaves the
		// leader dialling an address nobody answers just the same. Reporting
		// ready would let the rollout replace the next member and take the
		// quorum with it, so fail the startup instead: the pod restarts and
		// tries again, and the rollout stalls rather than losing quorum.
		_ = app.Close()
		return nil, fmt.Errorf("dqlite: could not reconcile this node's cluster membership: %w", err)
	}

	return readyOrClose(ctx, app)
}

func readyOrClose(ctx context.Context, app *dqliteapp.App) (*dqliteapp.App, error) {
	if err := app.Ready(ctx); err != nil {
		_ = app.Close()
		return nil, err
	}
	return app, nil
}

// discoveryCandidates returns the cached and configured addresses that leader
// discovery may fall back on. They are hints only — never a membership count.
func discoveryCandidates(dir string, seeds []string) (cached []string, configured []string) {
	store, err := client.NewYamlNodeStore(filepath.Join(dir, clusterFileName))
	if err != nil {
		return nil, seeds
	}
	members, err := store.Get(context.Background())
	if err != nil {
		return nil, seeds
	}
	for _, member := range members {
		cached = append(cached, member.Address)
	}
	return cached, seeds
}
