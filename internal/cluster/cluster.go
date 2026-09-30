// Package cluster reports the *liveness* of the dqlite raft cluster behind
// this Caesium process, as distinct from its configured membership.
//
// The console previously derived "quorum 3/3" from the length of the tracked
// node list, so a member that had been dead for minutes still counted towards
// availability (issue #494). Membership is what the cluster was told to be;
// liveness is what answers right now. This package keeps the two apart:
//
//	Members      – raft membership (address, role, leader) plus configured seeds
//	Reachability – per member, the result of an actual bounded dqlite RPC
//	Quorum       – how many VOTERS answered, versus how many a majority needs
//
// Probing is deliberately kept off the request path. `/health/ready` is the
// Kubernetes readiness probe target with a default one-second timeout, so a
// synchronous fan-out of dqlite RPCs there could remove serving pods from the
// endpoints. Snapshot returns the last observation immediately and refreshes
// in the background when it goes stale.
package cluster

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/pkg/dqlite"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/canonical/go-dqlite/v3/client"
)

// Reachability is the observed liveness of a single cluster member.
type Reachability string

const (
	// Reachable means the member answered a dqlite RPC within the probe budget.
	Reachable Reachability = "reachable"
	// Unreachable means the probe ran and the member did not answer.
	Unreachable Reachability = "unreachable"
	// Unknown means liveness was not determined — never render this as healthy.
	Unknown Reachability = "unknown"
)

// Status summarises quorum availability.
type Status string

const (
	// StatusAvailable means every voter answered.
	StatusAvailable Status = "available"
	// StatusDegraded means a majority of voters answered but at least one did not.
	StatusDegraded Status = "degraded"
	// StatusUnavailable means fewer voters answered than a majority requires.
	StatusUnavailable Status = "unavailable"
	// StatusUnknown means quorum could not be determined (membership or probe
	// results are missing). It is explicitly NOT healthy.
	StatusUnknown Status = "unknown"
)

// Role mirrors the dqlite raft roles, normalised to the spellings the console
// and `/v1/system/nodes` already use.
const (
	RoleVoter   = "voter"
	RoleStandby = "standby"
	RoleSpare   = "spare"
	RoleUnknown = "unknown"
)

// Member is one dqlite cluster member and its observed liveness.
type Member struct {
	ID           uint64       `json:"id,omitempty"`
	Address      string       `json:"address"`
	Role         string       `json:"role"`
	Leader       bool         `json:"leader"`
	Reachability Reachability `json:"reachability"`
	LatencyMs    *int64       `json:"latency_ms,omitempty"`
}

// Quorum separates configured membership from what is actually serving.
type Quorum struct {
	Status            Status `json:"status"`
	TotalVoters       int    `json:"total_voters"`
	ReachableVoters   int    `json:"reachable_voters"`
	UnreachableVoters int    `json:"unreachable_voters"`
	UnknownVoters     int    `json:"unknown_voters"`
	// RequiredVoters is the majority a raft cluster of TotalVoters needs.
	RequiredVoters int `json:"required_voters"`
	// Available reports that a majority of voters were observed reachable.
	Available bool `json:"available"`
	// Degraded reports that the cluster is serving with less than full
	// redundancy, or that redundancy could not be confirmed.
	Degraded      bool   `json:"degraded"`
	LeaderAddress string `json:"leader_address,omitempty"`
}

// NodeSummary reports liveness across ALL observed members, voters and
// non-voters alike.
//
// Quorum arithmetic is deliberately voter-only — a standby or spare replicates
// but cannot vote, so it can never make a cluster available. It can, however,
// be dead, and a dead node is a loss of redundancy that must show up somewhere:
// counting only voters here would let an unreachable standby sit next to "All
// systems operational", which is the same class of lie as issue #494 itself.
type NodeSummary struct {
	Status      Status `json:"status"`
	Total       int    `json:"total"`
	Reachable   int    `json:"reachable"`
	Unreachable int    `json:"unreachable"`
	Unknown     int    `json:"unknown"`
}

// View is a point-in-time observation of the cluster.
type View struct {
	// Clustered reports whether a dqlite raft cluster backs this process at
	// all. External databases (postgres) have no quorum to report.
	Clustered bool `json:"clustered"`
	// Observed reports whether a probe has ever completed. Until it has, every
	// member is Unknown and the quorum status is StatusUnknown.
	Observed   bool      `json:"observed"`
	ObservedAt time.Time `json:"observed_at,omitempty"`
	// Stale reports that ObservedAt is older than the refresh interval; a
	// refresh has been scheduled.
	Stale   bool     `json:"stale"`
	Members []Member `json:"members"`
	Quorum  Quorum   `json:"quorum"`
	// Nodes assesses every member's liveness, including non-voters, which
	// quorum deliberately ignores.
	Nodes NodeSummary `json:"nodes"`
}

// Status is the worst of the quorum assessment and the node assessment: a
// cluster that holds quorum but has lost a standby is degraded, not healthy.
func (v View) Status() Status {
	return worstStatus(v.Quorum.Status, v.Nodes.Status)
}

// worstStatus orders the vocabulary from best to worst so a summary can never
// be better than its worst component.
func worstStatus(a, b Status) Status {
	rank := func(s Status) int {
		switch s {
		case StatusAvailable:
			return 0
		case StatusDegraded:
			return 1
		case StatusUnavailable:
			return 3
		default: // StatusUnknown and anything unrecognised
			return 2
		}
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// MemberOf returns the observed member for an address, if the address is part
// of the raft cluster.
func (v View) MemberOf(address string) (Member, bool) {
	for _, m := range v.Members {
		if m.Address == address {
			return m, true
		}
	}
	return Member{}, false
}

const (
	// defaultRefreshInterval is how long an observation is served before a
	// background refresh is scheduled. The console polls every 15s, so a
	// failure surfaces within roughly one poll cycle.
	defaultRefreshInterval = 10 * time.Second
	// probeTimeout bounds a single member's dqlite RPC.
	probeTimeout = 2 * time.Second
	// maxConcurrentProbes caps how many probes are IN FLIGHT at once, so a
	// large membership cannot turn a refresh into a stampede. It bounds
	// parallelism only — every member is still probed, in waves.
	maxConcurrentProbes = 32
	// refreshBudget bounds one whole background refresh: membership plus every
	// wave of probes. It is generous because nothing waits on it — Snapshot
	// returns the previous observation immediately — and because a membership
	// larger than maxConcurrentProbes needs more than one wave.
	refreshBudget = 60 * time.Second
)

// membershipFunc reports raft membership and the current leader address.
// Replaced in tests.
var membershipFunc = liveMembership

// probeFunc reports whether one member answers. Replaced in tests.
var probeFunc = liveProbe

// clusteredFunc reports whether dqlite backs this process. Replaced in tests.
var clusteredFunc = dqliteEnabled

var (
	mu         sync.Mutex
	current    View
	refreshing bool
	// refreshInterval is a var only so a test can watch an observation change
	// without waiting a full interval. Production never writes it.
	refreshInterval = defaultRefreshInterval
)

// SetRefreshInterval shortens the background refresh interval and returns a
// function restoring the previous value. It exists so tests can observe a
// CHANGED observation propagate through the production Snapshot path rather
// than calling Refresh directly, which would bypass the very refresh they mean
// to exercise.
func SetRefreshInterval(d time.Duration) func() {
	mu.Lock()
	previous := refreshInterval
	refreshInterval = d
	mu.Unlock()

	return func() {
		mu.Lock()
		refreshInterval = previous
		mu.Unlock()
	}
}

// Snapshot returns the most recent observation without blocking, scheduling a
// background refresh when the observation is missing or stale. It never issues
// network calls on the caller's goroutine: `/health/ready` is the Kubernetes
// readiness probe, whose default timeout is one second.
func Snapshot() View {
	mu.Lock()
	view := current
	needsRefresh := !view.Observed || time.Since(view.ObservedAt) >= refreshInterval
	if needsRefresh {
		view.Stale = view.Observed
	}
	start := needsRefresh && !refreshing
	if start {
		refreshing = true
	}
	mu.Unlock()

	if !view.Observed {
		view = unobserved()
	}
	view.Members = append([]Member(nil), view.Members...)
	if start {
		go func() {
			// The budget covers membership plus every wave of probes, with
			// headroom so a slow membership call cannot cut probes short and
			// report a live member as unreachable.
			ctx, cancel := context.WithTimeout(context.Background(), refreshBudget)
			defer cancel()
			// The flag is cleared even if observe panics, so one bad refresh
			// cannot wedge the cache as permanently stale.
			defer func() {
				mu.Lock()
				refreshing = false
				mu.Unlock()
			}()

			observed := observe(ctx)

			mu.Lock()
			current = observed
			mu.Unlock()
		}()
	}
	return view
}

// Refresh observes the cluster synchronously and stores the result. It exists
// for callers that must not race the background refresh — tests and the
// integration surface.
func Refresh(ctx context.Context) View {
	observed := observe(ctx)

	mu.Lock()
	current = observed
	mu.Unlock()

	return observed
}

// reset clears the cached observation, waiting first for any background refresh
// to finish so a test can restore its stubs without racing the goroutine that
// is still reading them. Test-only.
func reset() {
	deadline := time.Now().Add(30 * time.Second)
	for {
		mu.Lock()
		idle := !refreshing
		if idle || time.Now().After(deadline) {
			current = View{}
			refreshing = false
			mu.Unlock()
			return
		}
		mu.Unlock()
		time.Sleep(time.Millisecond)
	}
}

func unobserved() View {
	clustered := clusteredFunc()
	return View{
		Clustered: clustered,
		Observed:  false,
		Members:   []Member{},
		Quorum: Quorum{
			Status:    StatusUnknown,
			Available: false,
			Degraded:  false,
		},
		Nodes: NodeSummary{Status: StatusUnknown},
	}
}

// observe fetches membership and probes every member in parallel.
func observe(ctx context.Context) View {
	if !clusteredFunc() {
		return View{
			Clustered:  false,
			Observed:   true,
			ObservedAt: time.Now().UTC(),
			Members:    []Member{},
			Quorum:     Quorum{Status: StatusUnknown},
			Nodes:      NodeSummary{Status: StatusUnknown},
		}
	}

	infos, leader, err := membershipFunc(ctx)
	if err != nil || len(infos) == 0 {
		if err != nil {
			log.Warn("cluster liveness: membership unavailable", "error", err)
		}
		// Membership itself is unknown. Fall back to the configured seeds so
		// the console still lists something, but every entry stays Unknown —
		// an unverified member must never be presented as healthy.
		seeds := seedMembers()
		return View{
			Clustered:  true,
			Observed:   true,
			ObservedAt: time.Now().UTC(),
			Members:    seeds,
			Quorum:     Quorum{Status: StatusUnknown},
			Nodes:      SummarizeNodes(seeds),
		}
	}

	members := make([]Member, 0, len(infos))
	for _, info := range infos {
		members = append(members, Member{
			ID:           info.ID,
			Address:      info.Address,
			Role:         normalizeRole(info.Role),
			Leader:       leader != "" && info.Address == leader,
			Reachability: Unknown,
		})
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Address < members[j].Address })

	probeMembers(ctx, members)

	return View{
		Clustered:  true,
		Observed:   true,
		ObservedAt: time.Now().UTC(),
		Members:    members,
		Quorum:     Summarize(members, leader),
		Nodes:      SummarizeNodes(members),
	}
}

// probeMembers probes EVERY member, with at most maxConcurrentProbes RPCs in
// flight at once.
//
// Concurrency is capped with a semaphore rather than by truncating the member
// list: an earlier version stopped the loop at the cap, so on a membership
// larger than the cap the members past it were never probed at all and stayed
// Unknown forever — which, since unknown liveness is never healthy, meant such
// a deployment could never report healthy. A cap on parallelism must not become
// a cap on coverage.
func probeMembers(ctx context.Context, members []Member) {
	sem := make(chan struct{}, maxConcurrentProbes)
	var wg sync.WaitGroup

	for i := range members {
		wg.Add(1)
		go func(m *Member) {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				// The refresh budget ran out before this member's turn. It
				// stays Unknown, which is honest: it was never probed.
				return
			}

			probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			if ctx.Err() != nil {
				// The refresh budget ended before this probe started. No member
				// liveness was measured, even if the semaphore happened to be
				// available when the canceled context won a select race.
				return
			}

			start := time.Now()
			if err := probeFunc(probeCtx, m.Address); err != nil {
				if ctx.Err() != nil {
					// A canceled refresh says nothing about this member. Keep
					// Unknown; only its own probe deadline or RPC failure proves
					// it did not answer within the member budget.
					return
				}
				log.Debug("cluster liveness probe failed", "address", m.Address, "error", err)
				m.Reachability = Unreachable
				return
			}
			latency := time.Since(start).Milliseconds()
			m.Reachability = Reachable
			m.LatencyMs = &latency
		}(&members[i])
	}
	wg.Wait()
}

// Summarize derives quorum availability from observed member liveness. It is
// the single place that decides what "available", "degraded" and "unavailable"
// mean, and it is deliberately pure so the decision table can be tested.
//
// Rules:
//   - only VOTERS count towards quorum; standby/spare members replicate but do
//     not vote, so they can never make a cluster available.
//   - a cluster is available when a strict majority of voters answered.
//   - it is degraded whenever it is serving with fewer than all voters, which
//     includes voters whose liveness is merely unknown.
//   - when the reachable voters alone fall short of a majority but the unknown
//     ones could close the gap, the answer is unknown, never available.
func Summarize(members []Member, leader string) Quorum {
	q := Quorum{LeaderAddress: leader}
	for _, m := range members {
		if m.Role != RoleVoter {
			continue
		}
		q.TotalVoters++
		switch m.Reachability {
		case Reachable:
			q.ReachableVoters++
		case Unreachable:
			q.UnreachableVoters++
		default:
			q.UnknownVoters++
		}
	}

	if q.TotalVoters == 0 {
		q.Status = StatusUnknown
		return q
	}

	q.RequiredVoters = q.TotalVoters/2 + 1

	switch {
	case q.ReachableVoters >= q.RequiredVoters:
		q.Available = true
		if q.ReachableVoters == q.TotalVoters {
			q.Status = StatusAvailable
		} else {
			q.Status = StatusDegraded
			q.Degraded = true
		}
	case q.ReachableVoters+q.UnknownVoters >= q.RequiredVoters:
		// Quorum might hold, but nothing observed says it does.
		q.Status = StatusUnknown
		q.Degraded = true
	default:
		q.Status = StatusUnavailable
		q.Degraded = true
	}
	return q
}

// SummarizeNodes assesses liveness across every observed member, whatever its
// role. Quorum ignores non-voters by design; this does not, so a crashed
// standby or spare still degrades cluster health instead of hiding behind an
// intact voter majority.
func SummarizeNodes(members []Member) NodeSummary {
	s := NodeSummary{Total: len(members)}
	for _, m := range members {
		switch m.Reachability {
		case Reachable:
			s.Reachable++
		case Unreachable:
			s.Unreachable++
		default:
			s.Unknown++
		}
	}

	switch {
	case s.Total == 0:
		s.Status = StatusUnknown
	case s.Unreachable > 0:
		// A member is known to be down. That is a loss of redundancy even when
		// the voters that remain still form a majority.
		s.Status = StatusDegraded
	case s.Unknown > 0:
		s.Status = StatusUnknown
	default:
		s.Status = StatusAvailable
	}
	return s
}

func normalizeRole(role client.NodeRole) string {
	switch role {
	case client.Voter:
		return RoleVoter
	case client.StandBy:
		return RoleStandby
	case client.Spare:
		return RoleSpare
	default:
		return RoleUnknown
	}
}

// seedMembers lists the configured cluster addresses with unknown liveness.
func seedMembers() []Member {
	seen := map[string]bool{}
	members := make([]Member, 0, 4)
	add := func(addr string) {
		addr = strings.TrimSpace(addr)
		if addr == "" || seen[addr] {
			return
		}
		seen[addr] = true
		members = append(members, Member{
			Address:      addr,
			Role:         RoleUnknown,
			Reachability: Unknown,
		})
	}
	v := env.Variables()
	add(v.NodeAddress)
	for _, addr := range v.DatabaseNodes {
		add(addr)
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Address < members[j].Address })
	return members
}

func dqliteEnabled() bool {
	dbType := strings.ToLower(strings.TrimSpace(env.Variables().DatabaseType))
	return dbType == "internal" || dbType == dqlite.DriverName
}

// liveMembership asks the LOCAL dqlite node for membership and the leader it
// currently believes in. Asking the local node (rather than searching for the
// leader) keeps the call cheap and keeps it answerable even while the cluster
// has no leader.
func liveMembership(ctx context.Context) ([]client.NodeInfo, string, error) {
	addr := strings.TrimSpace(env.Variables().NodeAddress)
	if addr == "" {
		return nil, "", nil
	}

	dialCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	cli, err := client.New(dialCtx, addr)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = cli.Close() }()

	members, err := cli.Cluster(dialCtx)
	if err != nil {
		return nil, "", err
	}

	leader := ""
	if info, err := cli.Leader(dialCtx); err == nil && info != nil {
		leader = info.Address
	}
	return members, leader, nil
}

// liveProbe issues one cheap dqlite RPC against a member. A member that cannot
// be dialled, or that dials but does not answer, is not serving.
func liveProbe(ctx context.Context, address string) error {
	cli, err := client.New(ctx, address)
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()

	_, err = cli.Leader(ctx)
	return err
}
