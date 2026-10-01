package dqlite

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/pkg/log"
	dqliteapp "github.com/canonical/go-dqlite/v3/app"
	"github.com/canonical/go-dqlite/v3/client"
)

// Removing the member a disk-loss replacement leaves behind (#582 follow-up).
//
// A pod replaced with an empty volume joins the cluster under a fresh node ID.
// go-dqlite's role adjustment then promotes the new member and demotes the lost
// member's entry to a spare, but nothing ever removes that entry: it stays in
// the raft configuration at its old address for as long as the cluster lives.
// It has no vote, yet it keeps every member's liveness report degraded (an
// unreachable spare is a lost node, see internal/cluster) and it holds its old
// address, which a later replacement could be given.
//
// RemoveStaleMember is the supported way to drop it. It deliberately refuses
// everything except the one shape that entry has: a non-voting member that
// does not answer, in a cluster whose voters are all healthy. The checks run
// against the leader's view, the removal goes through the leader, and a
// per-node mutex keeps two requests on the same node from interleaving their
// checks. Across nodes dqlite itself serializes configuration changes.

// RemovalReason is the machine-readable reason a member removal was refused.
type RemovalReason string

const (
	// RemovalNotClustered: this process is not running a dqlite node (an
	// external database backs it), so there is no raft membership to change.
	RemovalNotClustered RemovalReason = "not_clustered"
	// RemovalNoLeader: no leader could be reached or it stopped leading
	// mid-request. Retryable.
	RemovalNoLeader RemovalReason = "no_leader"
	// RemovalNotAMember: the leader's configuration has no member with this ID
	// (never a member, or already removed).
	RemovalNotAMember RemovalReason = "not_a_member"
	// RemovalLocalNode: the ID is the node serving the request.
	RemovalLocalNode RemovalReason = "local_node"
	// RemovalLeader: the ID is the current leader.
	RemovalLeader RemovalReason = "leader"
	// RemovalVoter: the member still votes. A lost member is demoted to spare
	// by go-dqlite's role adjustment (every 30 s by default) once enough live
	// voters exist; an unreachable voter is therefore retryable.
	RemovalVoter RemovalReason = "voter"
	// RemovalInsufficientVoters: the cluster has fewer than three voters, so
	// it has no fault tolerance to restore and membership is not changed.
	RemovalInsufficientVoters RemovalReason = "insufficient_voters"
	// RemovalReachable: a dqlite node answers at the member's address. Only a
	// member that is gone may be removed.
	RemovalReachable RemovalReason = "reachable"
	// RemovalVotersUnreachable: at least one voter does not answer. A cluster
	// that is already degraded is not reconfigured.
	RemovalVotersUnreachable RemovalReason = "voters_unreachable"
	// RemovalConfigurationChange: the leader is applying another membership
	// or role change and refuses overlapping ones. Retryable.
	RemovalConfigurationChange RemovalReason = "configuration_change_in_progress"
)

// minVotersForMemberRemoval is the smallest voter set membership may be changed
// in. Three is the smallest raft cluster that survives the loss of a member,
// and is what the chart deploys.
const minVotersForMemberRemoval = 3

// How hard to try before calling a member unreachable. go-dqlite's own role
// adjustment calls a member online when a connection plus a Describe round
// trip completes within 2 s; the same test is repeated here so that one lost
// packet cannot make a live member look gone. Variables only so tests can
// shorten them.
var (
	memberProbeAttempts = 3
	memberProbeTimeout  = 2 * time.Second
	memberProbeInterval = 250 * time.Millisecond
)

// memberRemovalLocks serializes the removals issued through one node, so two
// requests to the same server cannot both pass the checks before either
// removes. Keyed by app because the loopback tests run several nodes in one
// process; production has exactly one.
var memberRemovalLocks sync.Map // *dqliteapp.App -> *sync.Mutex

func memberRemovalLock(app *dqliteapp.App) *sync.Mutex {
	lock, _ := memberRemovalLocks.LoadOrStore(app, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// MemberRecord is a raft configuration entry as the leader records it.
type MemberRecord struct {
	ID      uint64
	Address string
	// Role is "voter", "standby" or "spare".
	Role   string
	Leader bool
	// Reachable is the probe result for the member a removal targeted; nil
	// when it was not probed.
	Reachable *bool
}

// MemberRemoval describes a completed removal.
type MemberRemoval struct {
	Removed MemberRecord
	Leader  MemberRecord
	// Members is the leader's configuration after the removal.
	Members []MemberRecord
}

// MemberRemovalRefusal is returned when a removal was not attempted, or was
// refused by the leader. The configuration is unchanged.
type MemberRemovalRefusal struct {
	ID uint64
	// Reason is the first applicable reason; Reasons lists every applicable
	// membership-state reason in the same order, so a caller can see all that
	// would have to change.
	Reason  RemovalReason
	Reasons []RemovalReason
	Detail  string
	// Retryable reports that the same request may succeed later without any
	// operator action: a configuration change in flight, a leader election,
	// or a lost voter that role adjustment has yet to demote.
	Retryable bool
	// Member is the target as the leader records it; nil when it is not a
	// member.
	Member *MemberRecord
	Leader *MemberRecord
}

func (r *MemberRemovalRefusal) Error() string {
	return fmt.Sprintf("dqlite: refusing to remove member %d: %s: %s", r.ID, r.Reason, r.Detail)
}

// RemoveStaleMember removes a lost member from the raft configuration through
// the current leader. It succeeds only when the member is a spare or standby,
// is not this node or the leader, does not answer at its address, and the
// cluster has at least three voters that all answer. Every other case returns a
// *MemberRemovalRefusal and changes nothing.
func RemoveStaleMember(ctx context.Context, id uint64) (MemberRemoval, error) {
	dqApp := currentApp.Load()
	if dqApp == nil {
		return MemberRemoval{}, &MemberRemovalRefusal{ID: id, Reason: RemovalNotClustered,
			Reasons: []RemovalReason{RemovalNotClustered},
			Detail:  "this process is not running a dqlite node; there is no raft membership to change"}
	}
	return removeStaleMember(ctx, dqApp, id)
}

func removeStaleMember(ctx context.Context, app *dqliteapp.App, id uint64) (MemberRemoval, error) {
	lock := memberRemovalLock(app)
	lock.Lock()
	defer lock.Unlock()

	connectCtx, cancel := context.WithTimeout(ctx, leaderConnectTimeout)
	cli, err := app.FindLeader(connectCtx)
	cancel()
	if err != nil {
		return MemberRemoval{}, noLeader(id, fmt.Sprintf("no dqlite leader reachable: %v", err))
	}
	defer func() { _ = cli.Close() }()

	leader, members, refusal := leaderView(ctx, cli, id)
	if refusal != nil {
		return MemberRemoval{}, refusal
	}
	target := memberByID(members, id)
	if target == nil {
		return MemberRemoval{}, notAMember(id, leader, members)
	}
	if refusal := refuseByMembership(ctx, *target, app.ID(), leader, members); refusal != nil {
		return MemberRemoval{}, refusal
	}

	// The member must be gone, not merely quiet on the leader's last round.
	if memberAnswers(ctx, target.Address) {
		reachable := true
		record := memberRecord(*target, leader.ID)
		record.Reachable = &reachable
		return MemberRemoval{}, &MemberRemovalRefusal{ID: id, Reason: RemovalReachable,
			Reasons: []RemovalReason{RemovalReachable},
			Detail: fmt.Sprintf("a dqlite node answers at %s; only a member that no longer answers may be removed "+
				"(if a new pod was given the lost member's address, delete that pod so it gets a new one)", target.Address),
			Member: &record, Leader: leaderRecord(leader)}
	}
	if err := ctx.Err(); err != nil {
		return MemberRemoval{}, err
	}
	var silent []string
	for _, m := range members {
		if m.Role == client.Voter && !memberAnswers(ctx, m.Address) {
			silent = append(silent, fmt.Sprintf("%d@%s", m.ID, m.Address))
		}
	}
	if err := ctx.Err(); err != nil {
		return MemberRemoval{}, err
	}
	if len(silent) > 0 {
		unreachable := false
		record := memberRecord(*target, leader.ID)
		record.Reachable = &unreachable
		return MemberRemoval{}, &MemberRemovalRefusal{ID: id, Reason: RemovalVotersUnreachable,
			Reasons: []RemovalReason{RemovalVotersUnreachable},
			Detail: fmt.Sprintf("voters %s do not answer; restore every voter before changing membership",
				strings.Join(silent, ", ")),
			Member: &record, Leader: leaderRecord(leader)}
	}

	// The probes take seconds. Judge the configuration the removal will
	// actually apply to, not the one read before them.
	leader, members, refusal = leaderView(ctx, cli, id)
	if refusal != nil {
		return MemberRemoval{}, refusal
	}
	target = memberByID(members, id)
	if target == nil {
		return MemberRemoval{}, notAMember(id, leader, members)
	}
	if refusal := refuseByMembership(ctx, *target, app.ID(), leader, members); refusal != nil {
		return MemberRemoval{}, refusal
	}
	removed := memberRecord(*target, leader.ID)
	unreachable := false
	removed.Reachable = &unreachable

	if err := cli.Remove(ctx, id); err != nil {
		return MemberRemoval{}, classifyRemoveError(ctx, cli, id, removed, leader, err)
	}

	after, err := cli.Cluster(ctx)
	if err != nil {
		return MemberRemoval{}, fmt.Errorf("dqlite: removed member %d but could not confirm it: %w", id, err)
	}
	if memberByID(after, id) != nil {
		return MemberRemoval{}, fmt.Errorf("dqlite: member %d is still in the configuration after its removal", id)
	}
	log.Warn("removed a stale member from the dqlite cluster configuration",
		"node_id", id, "node_address", removed.Address, "role", removed.Role,
		"leader_id", leader.ID, "leader_address", leader.Address, "members", len(after))
	return MemberRemoval{Removed: removed, Leader: *leaderRecord(leader), Members: memberRecords(after, leader.ID)}, nil
}

// leaderView reads the leader's identity and its configuration over cli.
func leaderView(ctx context.Context, cli *client.Client, id uint64) (client.NodeInfo, []client.NodeInfo, *MemberRemovalRefusal) {
	// A node that knows of no leader answers with ID 0 rather than an error.
	leader, err := cli.Leader(ctx)
	if err != nil || leader == nil || leader.ID == 0 {
		return client.NodeInfo{}, nil, noLeader(id, fmt.Sprintf("could not read the dqlite leader (leader %v): %v", leader, err))
	}
	members, err := cli.Cluster(ctx)
	if err != nil {
		return client.NodeInfo{}, nil, noLeader(id, fmt.Sprintf("could not read the leader's membership: %v", err))
	}
	return *leader, members, nil
}

// removalRefusals lists every membership-state reason target may not be
// removed, in precedence order. It is pure so the decision table can be tested
// without a cluster; reachability is probed separately.
func removalRefusals(target client.NodeInfo, localID, leaderID uint64, members []client.NodeInfo) []RemovalReason {
	var reasons []RemovalReason
	if target.ID == localID {
		reasons = append(reasons, RemovalLocalNode)
	}
	if target.ID == leaderID {
		reasons = append(reasons, RemovalLeader)
	}
	if target.Role == client.Voter {
		reasons = append(reasons, RemovalVoter)
	}
	voters := 0
	for _, m := range members {
		if m.ID != target.ID && m.Role == client.Voter {
			voters++
		}
	}
	if voters < minVotersForMemberRemoval {
		reasons = append(reasons, RemovalInsufficientVoters)
	}
	return reasons
}

// refuseByMembership turns removalRefusals into a refusal. The one retryable
// case is a lost voter that role adjustment has yet to demote, so only then is
// the target probed.
func refuseByMembership(ctx context.Context, target client.NodeInfo, localID uint64, leader client.NodeInfo, members []client.NodeInfo) *MemberRemovalRefusal {
	reasons := removalRefusals(target, localID, leader.ID, members)
	if len(reasons) == 0 {
		return nil
	}
	record := memberRecord(target, leader.ID)
	refusal := &MemberRemovalRefusal{ID: target.ID, Reason: reasons[0], Reasons: reasons,
		Member: &record, Leader: leaderRecord(leader)}
	switch reasons[0] {
	case RemovalLocalNode:
		refusal.Detail = "this is the node serving the request; a node never removes itself"
	case RemovalLeader:
		refusal.Detail = "this member is the current leader"
	case RemovalVoter:
		reachable := memberAnswers(ctx, target.Address)
		record.Reachable = &reachable
		if !reachable && len(reasons) == 1 {
			refusal.Retryable = true
			refusal.Detail = fmt.Sprintf("member is still a voter but does not answer at %s; go-dqlite's role "+
				"adjustment demotes a lost voter to spare once three live voters exist (every 30 s by default); retry then",
				target.Address)
			break
		}
		refusal.Detail = "member is a voter; only a demoted (spare or standby) member may be removed"
	case RemovalInsufficientVoters:
		refusal.Detail = fmt.Sprintf("removal requires at least %d other voters; the configuration has fewer",
			minVotersForMemberRemoval)
	}
	return refusal
}

// classifyRemoveError maps a failed raft removal to a refusal wherever the
// cause is a known, benign state, re-reading the configuration to tell a lost
// race apart from a real failure.
func classifyRemoveError(ctx context.Context, cli *client.Client, id uint64, target MemberRecord, leader client.NodeInfo, err error) error {
	if strings.Contains(err.Error(), errConfigurationChangeInFlight) {
		return &MemberRemovalRefusal{ID: id, Reason: RemovalConfigurationChange,
			Reasons: []RemovalReason{RemovalConfigurationChange}, Retryable: true,
			Detail: "the leader is applying another membership or role change and refuses overlapping ones; " +
				"retry shortly (a promotion stalled on an unreachable member holds changes off for up to about a minute)",
			Member: &target, Leader: leaderRecord(leader)}
	}
	if current, readErr := cli.Cluster(ctx); readErr == nil && memberByID(current, id) == nil {
		return notAMember(id, leader, current)
	}
	if isNotLeader(err) {
		return noLeader(id, fmt.Sprintf("leadership changed during the removal: %v", err))
	}
	return fmt.Errorf("dqlite: remove member %d: %w", id, err)
}

// isNotLeader recognizes raft's RAFT_NOTLEADER ("server is not the leader")
// and RAFT_LEADERSHIPLOST answers, which go-dqlite surfaces only as text.
func isNotLeader(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not the leader") || strings.Contains(msg, "not leader") ||
		strings.Contains(msg, "leadership lost")
}

func noLeader(id uint64, detail string) *MemberRemovalRefusal {
	return &MemberRemovalRefusal{ID: id, Reason: RemovalNoLeader, Reasons: []RemovalReason{RemovalNoLeader},
		Retryable: true, Detail: detail}
}

func notAMember(id uint64, leader client.NodeInfo, members []client.NodeInfo) *MemberRemovalRefusal {
	return &MemberRemovalRefusal{ID: id, Reason: RemovalNotAMember, Reasons: []RemovalReason{RemovalNotAMember},
		Detail: fmt.Sprintf("the leader's configuration (%d members) has no member %d; it was never a member or is already removed",
			len(members), id),
		Leader: leaderRecord(leader)}
}

// memberAnswers reports whether a dqlite node answers at address. Any node
// answering counts: the protocol cannot say which node ID is listening, so an
// address reused by a live pod is treated as live.
func memberAnswers(ctx context.Context, address string) bool {
	for attempt := 0; attempt < memberProbeAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(memberProbeInterval):
			}
		}
		probeCtx, cancel := context.WithTimeout(ctx, memberProbeTimeout)
		cli, err := client.New(probeCtx, address)
		if err == nil {
			_, err = cli.Describe(probeCtx)
			_ = cli.Close()
		}
		cancel()
		if err == nil {
			return true
		}
	}
	return false
}

// RoleName spells a raft role the way the API and console do.
func RoleName(role client.NodeRole) string {
	switch role {
	case client.Voter:
		return "voter"
	case client.StandBy:
		return "standby"
	case client.Spare:
		return "spare"
	default:
		return "unknown"
	}
}

func memberRecord(node client.NodeInfo, leaderID uint64) MemberRecord {
	return MemberRecord{ID: node.ID, Address: node.Address, Role: RoleName(node.Role), Leader: node.ID == leaderID}
}

func leaderRecord(leader client.NodeInfo) *MemberRecord {
	if leader.ID == 0 {
		return nil
	}
	record := memberRecord(leader, leader.ID)
	return &record
}

func memberRecords(nodes []client.NodeInfo, leaderID uint64) []MemberRecord {
	out := make([]MemberRecord, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, memberRecord(n, leaderID))
	}
	return out
}
