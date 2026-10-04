package lifecycle

// The disk-loss membership checks are pure so the unit lane can exercise them
// without kind. This file is deliberately untagged: the integration-tagged
// runners (TestLifecycleClusterOrdinalZeroLoss, TestLifecycleClusterJoiningOrdinalOne
// and their *Removed phases) call the same helpers, and their -test.run
// anchors never select the table tests below.

import (
	"cmp"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	dqclient "github.com/canonical/go-dqlite/v3/client"
	"github.com/stretchr/testify/require"
)

// dqliteBootstrapID is go-dqlite's fixed ID for the node that bootstraps a
// cluster (dqlite.BootstrapID). Repeated here so the runner needs no cgo.
const dqliteBootstrapID uint64 = 3297041220608546238

// ordinalZeroView is one member's direct Cluster RPC answer. Members is the
// recorded, sorted "id/address/role" form; nodes feeds the checker.
type ordinalZeroView struct {
	Name    string   `json:"name"`
	Address string   `json:"address"`
	Leader  string   `json:"leader"`
	Members []string `json:"members"`
	nodes   []dqclient.NodeInfo
}

func newOrdinalZeroView(name, address string, leader dqclient.NodeInfo, nodes []dqclient.NodeInfo) ordinalZeroView {
	view := ordinalZeroView{Name: name, Address: address, Leader: fmt.Sprintf("%d/%s", leader.ID, leader.Address)}
	view.nodes = append([]dqclient.NodeInfo(nil), nodes...)
	slices.SortFunc(view.nodes, func(a, b dqclient.NodeInfo) int { return cmp.Compare(a.ID, b.ID) })
	for _, n := range view.nodes {
		view.Members = append(view.Members, fmt.Sprintf("%d/%s/%s", n.ID, n.Address, strings.ToLower(n.Role.String())))
	}
	sort.Strings(view.Members)
	return view
}

type ordinalZeroEntry struct {
	ID      uint64 `json:"id"`
	Address string `json:"address"`
	Role    string `json:"role"`
}

// ordinalZeroMembership summarises the first view's membership for the record.
type ordinalZeroMembership struct {
	VoterAddresses []string           `json:"voter_addresses"`
	StaleEntries   []ordinalZeroEntry `json:"stale_entries"`
}

// ordinalZeroSettled reports why the observed direct Cluster RPC views do not
// yet show the recovered three-voter cluster. Every view must agree on one
// leader and one membership whose voters are exactly the live members: one
// voter per live address, the fresh node among them. Every other entry is
// stale (an address no live pod serves, or a lost member's ID) and must be
// absent or non-voting. A stale voter leaves four voters needing three votes
// from three live members, so losing any one of them would lose quorum.
func ordinalZeroSettled(views []ordinalZeroView, freshID uint64, freshAddr string, liveAddrs []string, staleIDs []uint64) (ordinalZeroMembership, error) {
	var summary ordinalZeroMembership
	if len(views) == 0 || len(views) != len(liveAddrs) {
		return summary, fmt.Errorf("observed %d of %d members", len(views), len(liveAddrs))
	}
	live := map[string]bool{}
	for _, addr := range liveAddrs {
		live[addr] = true
	}
	lost := map[uint64]bool{}
	for _, id := range staleIDs {
		if id != 0 {
			lost[id] = true
		}
	}
	var staleVoters []string
	voterAt := map[string]int{}
	freshVotes := false
	for _, n := range views[0].nodes {
		voter := n.Role == dqclient.Voter
		if voter {
			summary.VoterAddresses = append(summary.VoterAddresses, n.Address)
		}
		if !live[n.Address] || lost[n.ID] {
			summary.StaleEntries = append(summary.StaleEntries,
				ordinalZeroEntry{ID: n.ID, Address: n.Address, Role: strings.ToLower(n.Role.String())})
			if voter {
				staleVoters = append(staleVoters, fmt.Sprintf("%d/%s", n.ID, n.Address))
			}
			continue
		}
		if voter {
			voterAt[n.Address]++
			if n.ID == freshID && n.Address == freshAddr {
				freshVotes = true
			}
		}
	}
	sort.Strings(summary.VoterAddresses)
	for _, v := range views {
		if v.Leader != views[0].Leader {
			return summary, fmt.Errorf("%s reports leader %s but %s reports %s", v.Name, v.Leader, views[0].Name, views[0].Leader)
		}
		if strings.Join(v.Members, ",") != strings.Join(views[0].Members, ",") {
			return summary, fmt.Errorf("%s membership %v differs from %s membership %v", v.Name, v.Members, views[0].Name, views[0].Members)
		}
	}
	if !freshVotes {
		return summary, fmt.Errorf("fresh node %d/%s is not a voter in the shared membership %v", freshID, freshAddr, views[0].Members)
	}
	if len(staleVoters) > 0 {
		return summary, fmt.Errorf("stale entries %v still vote (%d voters for %d live members, so fault tolerance is not restored): %v",
			staleVoters, len(summary.VoterAddresses), len(liveAddrs), views[0].Members)
	}
	for _, addr := range liveAddrs {
		if voterAt[addr] != 1 {
			return summary, fmt.Errorf("live member %s has %d voter entries, want exactly 1: %v", addr, voterAt[addr], views[0].Members)
		}
	}
	if len(summary.VoterAddresses) != len(liveAddrs) {
		return summary, fmt.Errorf("%d voters for %d live members: %v", len(summary.VoterAddresses), len(liveAddrs), views[0].Members)
	}
	return summary, nil
}

// clusterHealthView is one member's /health checks.cluster answer, reduced to
// what the post-removal check reads.
type clusterHealthView struct {
	Name    string                `json:"name"`
	Status  string                `json:"status"`
	Members []clusterHealthMember `json:"members"`
}

type clusterHealthMember struct {
	ID           uint64 `json:"id"`
	Address      string `json:"address"`
	Role         string `json:"role"`
	Reachability string `json:"reachability"`
}

// staleEntriesRemoved reports why the observed views do not yet show the stale
// entries removed (H1, #582 follow-up). Every member's direct Cluster RPC must
// agree on one leader and one membership that is exactly the live members, one
// voter per live address, with no stale ID and no address no live pod serves.
// Every member's /health cluster view, which is what an operator and the
// console read, must list exactly the same IDs at the same addresses.
func staleEntriesRemoved(views []ordinalZeroView, health []clusterHealthView, liveAddrs []string, staleIDs []uint64) error {
	if len(views) == 0 || len(views) != len(liveAddrs) || len(health) != len(liveAddrs) {
		return fmt.Errorf("observed %d direct and %d /health views of %d members", len(views), len(health), len(liveAddrs))
	}
	stale := map[uint64]bool{}
	for _, id := range staleIDs {
		if id != 0 {
			stale[id] = true
		}
	}
	live := map[string]bool{}
	for _, addr := range liveAddrs {
		live[addr] = true
	}
	for _, v := range views {
		if v.Leader != views[0].Leader {
			return fmt.Errorf("%s reports leader %s but %s reports %s", v.Name, v.Leader, views[0].Name, views[0].Leader)
		}
		if strings.Join(v.Members, ",") != strings.Join(views[0].Members, ",") {
			return fmt.Errorf("%s membership %v differs from %s membership %v", v.Name, v.Members, views[0].Name, views[0].Members)
		}
	}
	want := map[uint64]string{}
	voterAt := map[string]int{}
	for _, n := range views[0].nodes {
		entry := fmt.Sprintf("%d/%s/%s", n.ID, n.Address, strings.ToLower(n.Role.String()))
		switch {
		case stale[n.ID]:
			return fmt.Errorf("stale entry %s is still in the configuration: %v", entry, views[0].Members)
		case !live[n.Address]:
			return fmt.Errorf("entry %s is at an address no live member serves: %v", entry, views[0].Members)
		case n.Role != dqclient.Voter:
			return fmt.Errorf("live member %s is not a voter: %v", entry, views[0].Members)
		}
		voterAt[n.Address]++
		want[n.ID] = n.Address
	}
	for _, addr := range liveAddrs {
		if voterAt[addr] != 1 {
			return fmt.Errorf("live member %s has %d voter entries, want exactly 1: %v", addr, voterAt[addr], views[0].Members)
		}
	}
	for _, h := range health {
		got := map[uint64]string{}
		for _, m := range h.Members {
			if stale[m.ID] {
				return fmt.Errorf("%s /health still lists stale entry %d at %s", h.Name, m.ID, m.Address)
			}
			got[m.ID] = m.Address
		}
		if !reflect.DeepEqual(got, want) {
			return fmt.Errorf("%s /health lists members %v, the configuration is %v", h.Name, got, want)
		}
	}
	return nil
}

type staleRemovalTarget struct {
	ID      string `json:"id"`
	Address string `json:"address"`
	Role    string `json:"role"`
}

// staleRemovalPlan is what the first phase hands the host.
type staleRemovalPlan struct {
	LifecycleID string               `json:"lifecycle_id"`
	Case        string               `json:"case"`
	Stage       string               `json:"stage"`
	Executor    string               `json:"executor"`
	Leader      staleRemovalTarget   `json:"leader"`
	Voter       staleRemovalTarget   `json:"voter"`
	Stale       []staleRemovalTarget `json:"stale"`
	StaleIDs    []string             `json:"stale_ids"`
	LiveAddrs   []string             `json:"live_addresses"`
	Detail      string               `json:"detail"`
	// Evidence is the first phase's observations, carried into the case
	// record verbatim. It stays raw so 64-bit node IDs are never rounded
	// through float64 on the way.
	Evidence json.RawMessage `json:"evidence"`
}

// staleRemovalAttempt is one `caesium system nodes remove` the host ran.
type staleRemovalAttempt struct {
	N      int    `json:"n"`
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	RC     int    `json:"rc"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

type staleRemovalHost struct {
	LifecycleID string                `json:"lifecycle_id"`
	Stage       string                `json:"stage"`
	Executor    string                `json:"executor"`
	Attempts    []staleRemovalAttempt `json:"attempts"`
}

// removalAnswer is the JSON `caesium system nodes remove --json` prints.
type removalAnswer struct {
	Status    string `json:"status"`
	Reason    string `json:"reason"`
	Retryable bool   `json:"retryable"`
	Removed   struct {
		ID      string `json:"id"`
		Address string `json:"address"`
		Role    string `json:"role"`
	} `json:"removed"`
	Members []struct {
		ID   string `json:"id"`
		Role string `json:"role"`
	} `json:"members"`
}

// lastAttempt returns the final attempt the host made for one kind and ID.
func lastAttempt(attempts []staleRemovalAttempt, kind, id string) (staleRemovalAttempt, int, bool) {
	var last staleRemovalAttempt
	count := 0
	for _, a := range attempts {
		if a.Kind == kind && a.ID == id {
			last = a
			count++
		}
	}
	return last, count, count > 0
}

// judgeRemovalAttempts checks the host's CLI invocations: both controls
// refused with the exact reason and a non-zero exit, and every stale entry
// removed on its last attempt after nothing but retryable refusals. It returns
// a blocked reason when an answer is unobservable and a failure when the
// command demonstrably did the wrong thing.
func judgeRemovalAttempts(plan staleRemovalPlan, host staleRemovalHost) (blocked, failed string) {
	parse := func(a staleRemovalAttempt) (removalAnswer, bool) {
		var answer removalAnswer
		if err := json.Unmarshal([]byte(a.Stdout), &answer); err != nil || answer.Status == "" {
			return removalAnswer{}, false
		}
		return answer, true
	}
	for _, control := range []struct {
		kind, reason string
		target       staleRemovalTarget
	}{{"control-leader", "leader", plan.Leader}, {"control-voter", "voter", plan.Voter}} {
		a, _, ok := lastAttempt(host.Attempts, control.kind, control.target.ID)
		if !ok {
			return fmt.Sprintf("no %s attempt was recorded for %s", control.kind, control.target.ID), ""
		}
		answer, ok := parse(a)
		if !ok {
			return fmt.Sprintf("%s attempt on %s printed no JSON answer (rc %d): %q / %q", control.kind, control.target.ID, a.RC, a.Stdout, a.Stderr), ""
		}
		if a.RC == 0 || answer.Status != "refused" || answer.Reason != control.reason {
			return "", fmt.Sprintf("removing the live %s %s was not refused with %q: rc %d, answer %s", control.reason, control.target.ID, control.reason, a.RC, a.Stdout)
		}
		if !strings.Contains(a.Stderr, "refused to remove dqlite member "+control.target.ID) {
			return "", fmt.Sprintf("the %s refusal did not reach stderr: %q", control.kind, a.Stderr)
		}
	}
	for _, stale := range plan.Stale {
		a, count, ok := lastAttempt(host.Attempts, "stale", stale.ID)
		if !ok {
			return fmt.Sprintf("no removal attempt was recorded for stale entry %s", stale.ID), ""
		}
		for _, earlier := range host.Attempts {
			if earlier.Kind != "stale" || earlier.ID != stale.ID || earlier.N == a.N {
				continue
			}
			if answer, ok := parse(earlier); !ok || !answer.Retryable {
				return "", fmt.Sprintf("stale entry %s was retried after a non-retryable answer (rc %d): %s", stale.ID, earlier.RC, earlier.Stdout)
			}
		}
		answer, ok := parse(a)
		if !ok {
			return fmt.Sprintf("removal of %s printed no JSON answer after %d attempts (rc %d): %q / %q", stale.ID, count, a.RC, a.Stdout, a.Stderr), ""
		}
		if a.RC != 0 || answer.Status != "removed" || answer.Removed.ID != stale.ID {
			return "", fmt.Sprintf("stale entry %s was not removed after %d attempts: rc %d, answer %s", stale.ID, count, a.RC, a.Stdout)
		}
	}
	return "", ""
}

func TestOrdinalZeroSettledRequiresExactlyTheLiveVoters(t *testing.T) {
	const (
		freshID  uint64 = 5396041916851386528
		oneID    uint64 = 1096228854381126167
		twoID    uint64 = 10861389684719149549
		oldOneID uint64 = 15002730754803141367
		fresh           = "10.244.2.9:9001"
		one             = "10.244.1.10:9001"
		two             = "10.244.3.11:9001"
		oldZero         = "10.244.2.5:9001"
		oldOne          = "10.244.1.7:9001"
	)
	live := []string{fresh, one, two}
	stale := []uint64{dqliteBootstrapID, oldOneID}
	leader := dqclient.NodeInfo{ID: twoID, Address: two}
	liveVoters := []dqclient.NodeInfo{
		{ID: freshID, Address: fresh, Role: dqclient.Voter},
		{ID: oneID, Address: one, Role: dqclient.Voter},
		{ID: twoID, Address: two, Role: dqclient.Voter},
	}
	with := func(extra ...dqclient.NodeInfo) []dqclient.NodeInfo {
		return append(append([]dqclient.NodeInfo(nil), liveVoters...), extra...)
	}
	observe := func(nodes []dqclient.NodeInfo) []ordinalZeroView {
		return []ordinalZeroView{
			newOrdinalZeroView("caesium-0", fresh, leader, nodes),
			newOrdinalZeroView("caesium-1", one, leader, nodes),
			newOrdinalZeroView("caesium-2", two, leader, nodes),
		}
	}
	cases := []struct {
		name      string
		nodes     []dqclient.NodeInfo
		wantErr   string // empty: settled
		wantStale []ordinalZeroEntry
	}{
		{name: "three live voters plus the stale bootstrap voter", wantErr: "still vote",
			nodes:     with(dqclient.NodeInfo{ID: dqliteBootstrapID, Address: oldZero, Role: dqclient.Voter}),
			wantStale: []ordinalZeroEntry{{ID: dqliteBootstrapID, Address: oldZero, Role: "voter"}}},
		{name: "three live voters plus the stale bootstrap spare",
			nodes:     with(dqclient.NodeInfo{ID: dqliteBootstrapID, Address: oldZero, Role: dqclient.Spare}),
			wantStale: []ordinalZeroEntry{{ID: dqliteBootstrapID, Address: oldZero, Role: "spare"}}},
		{name: "both stale entries non-voting",
			nodes: with(dqclient.NodeInfo{ID: dqliteBootstrapID, Address: oldZero, Role: dqclient.Spare},
				dqclient.NodeInfo{ID: oldOneID, Address: oldOne, Role: dqclient.StandBy}),
			wantStale: []ordinalZeroEntry{{ID: dqliteBootstrapID, Address: oldZero, Role: "spare"},
				{ID: oldOneID, Address: oldOne, Role: "stand-by"}}},
		{name: "three live voters, stale entries absent", nodes: with()},
		{name: "only two live voters", wantErr: "has 0 voter entries",
			nodes: []dqclient.NodeInfo{liveVoters[0], liveVoters[1], {ID: twoID, Address: two, Role: dqclient.Spare},
				{ID: dqliteBootstrapID, Address: oldZero, Role: dqclient.Spare}},
			wantStale: []ordinalZeroEntry{{ID: dqliteBootstrapID, Address: oldZero, Role: "spare"}}},
		{name: "stale ID voting at a reused live address", wantErr: "still vote",
			nodes:     []dqclient.NodeInfo{liveVoters[0], liveVoters[1], {ID: oldOneID, Address: two, Role: dqclient.Voter}},
			wantStale: []ordinalZeroEntry{{ID: oldOneID, Address: two, Role: "voter"}}},
		{name: "fresh node not yet promoted", wantErr: "is not a voter",
			nodes: []dqclient.NodeInfo{{ID: freshID, Address: fresh, Role: dqclient.Spare}, liveVoters[1], liveVoters[2],
				{ID: dqliteBootstrapID, Address: oldZero, Role: dqclient.Voter}},
			wantStale: []ordinalZeroEntry{{ID: dqliteBootstrapID, Address: oldZero, Role: "voter"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			summary, err := ordinalZeroSettled(observe(tc.nodes), freshID, fresh, live, stale)
			if tc.wantErr == "" {
				require.NoError(t, err)
				require.Equal(t, []string{one, fresh, two}, summary.VoterAddresses)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
			}
			require.Equal(t, tc.wantStale, summary.StaleEntries)
		})
	}

	t.Run("views disagree on the stale entry's role", func(t *testing.T) {
		views := observe(with(dqclient.NodeInfo{ID: dqliteBootstrapID, Address: oldZero, Role: dqclient.Spare}))
		views[1] = newOrdinalZeroView("caesium-1", one, leader,
			with(dqclient.NodeInfo{ID: dqliteBootstrapID, Address: oldZero, Role: dqclient.Voter}))
		_, err := ordinalZeroSettled(views, freshID, fresh, live, stale)
		require.ErrorContains(t, err, "differs from")
	})

	t.Run("a member left unobserved", func(t *testing.T) {
		_, err := ordinalZeroSettled(observe(with())[:2], freshID, fresh, live, stale)
		require.ErrorContains(t, err, "observed 2 of 3")
	})
}

func TestStaleEntriesRemovedRequiresEveryViewClean(t *testing.T) {
	const (
		freshID  uint64 = 5396041916851386528
		oneID    uint64 = 1096228854381126167
		twoID    uint64 = 10861389684719149549
		oldOneID uint64 = 15002730754803141367
		fresh           = "10.244.2.9:9001"
		one             = "10.244.1.10:9001"
		two             = "10.244.3.11:9001"
		oldZero         = "10.244.2.5:9001"
		oldOne          = "10.244.1.7:9001"
	)
	live := []string{fresh, one, two}
	stale := []uint64{dqliteBootstrapID, oldOneID}
	leader := dqclient.NodeInfo{ID: twoID, Address: two}
	liveVoters := []dqclient.NodeInfo{
		{ID: freshID, Address: fresh, Role: dqclient.Voter},
		{ID: oneID, Address: one, Role: dqclient.Voter},
		{ID: twoID, Address: two, Role: dqclient.Voter},
	}
	staleSpare := dqclient.NodeInfo{ID: dqliteBootstrapID, Address: oldZero, Role: dqclient.Spare}
	with := func(extra ...dqclient.NodeInfo) []dqclient.NodeInfo {
		return append(append([]dqclient.NodeInfo(nil), liveVoters...), extra...)
	}
	direct := func(nodes []dqclient.NodeInfo) []ordinalZeroView {
		return []ordinalZeroView{
			newOrdinalZeroView("caesium-0", fresh, leader, nodes),
			newOrdinalZeroView("caesium-1", one, leader, nodes),
			newOrdinalZeroView("caesium-2", two, leader, nodes),
		}
	}
	healthOf := func(nodes []dqclient.NodeInfo) []clusterHealthView {
		members := make([]clusterHealthMember, 0, len(nodes))
		for _, n := range nodes {
			members = append(members, clusterHealthMember{ID: n.ID, Address: n.Address, Role: strings.ToLower(n.Role.String())})
		}
		return []clusterHealthView{{Name: "caesium-0", Members: members}, {Name: "caesium-1", Members: members}, {Name: "caesium-2", Members: members}}
	}

	cases := []struct {
		name    string
		views   []ordinalZeroView
		health  []clusterHealthView
		wantErr string // empty: removed everywhere
	}{
		{name: "stale entries gone from every view", views: direct(with()), health: healthOf(with())},
		{name: "the stale spare is still configured", wantErr: "is still in the configuration",
			views: direct(with(staleSpare)), health: healthOf(with(staleSpare))},
		{name: "configuration clean but one /health view still lists the stale spare", wantErr: "caesium-1 /health still lists stale entry",
			views: direct(with()), health: func() []clusterHealthView {
				h := healthOf(with())
				h[1] = healthOf(with(staleSpare))[1]
				return h
			}()},
		{name: "one member has not applied the removal", wantErr: "differs from",
			views: func() []ordinalZeroView {
				v := direct(with())
				v[2] = newOrdinalZeroView("caesium-2", two, leader, with(staleSpare))
				return v
			}(), health: healthOf(with())},
		{name: "an unknown leftover at a dead address", wantErr: "no live member serves",
			views:  direct(with(dqclient.NodeInfo{ID: 77, Address: oldOne, Role: dqclient.Spare})),
			health: healthOf(with(dqclient.NodeInfo{ID: 77, Address: oldOne, Role: dqclient.Spare}))},
		{name: "a live member lost its vote", wantErr: "is not a voter",
			views:  direct([]dqclient.NodeInfo{liveVoters[0], liveVoters[1], {ID: twoID, Address: two, Role: dqclient.Spare}}),
			health: healthOf([]dqclient.NodeInfo{liveVoters[0], liveVoters[1], {ID: twoID, Address: two, Role: dqclient.Spare}})},
		{name: "a /health view missing a live member", wantErr: "the configuration is",
			views: direct(with()), health: func() []clusterHealthView {
				h := healthOf(with())
				h[0].Members = h[0].Members[:2]
				return h
			}()},
		{name: "a /health view left unobserved", wantErr: "observed 3 direct and 2 /health views",
			views: direct(with()), health: healthOf(with())[:2]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := staleEntriesRemoved(tc.views, tc.health, live, stale)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestJudgeRemovalAttemptsRequiresRefusedControlsAndRemovedStaleEntries(t *testing.T) {
	plan := staleRemovalPlan{
		Executor: "caesium-1",
		Leader:   staleRemovalTarget{ID: "10861389684719149549", Address: "10.244.3.11:9001", Role: "voter"},
		Voter:    staleRemovalTarget{ID: "5396041916851386528", Address: "10.244.2.9:9001", Role: "voter"},
		Stale:    []staleRemovalTarget{{ID: "3297041220608546238", Address: "10.244.2.5:9001", Role: "spare"}},
	}
	refused := func(n int, kind, id, reason string, retryable bool) staleRemovalAttempt {
		return staleRemovalAttempt{N: n, Kind: kind, ID: id, RC: 1,
			Stdout: fmt.Sprintf(`{"status":"refused","id":%q,"reason":%q,"retryable":%t}`, id, reason, retryable),
			Stderr: "Error: refused to remove dqlite member " + id + " (HTTP 409, " + reason + "): no"}
	}
	removed := func(n int, id string) staleRemovalAttempt {
		return staleRemovalAttempt{N: n, Kind: "stale", ID: id,
			Stdout: fmt.Sprintf(`{"status":"removed","removed":{"id":%q,"address":"10.244.2.5:9001","role":"spare"}}`, id)}
	}
	good := []staleRemovalAttempt{
		refused(1, "control-leader", plan.Leader.ID, "leader", false),
		refused(2, "control-voter", plan.Voter.ID, "voter", false),
		refused(3, "stale", plan.Stale[0].ID, "configuration_change_in_progress", true),
		removed(4, plan.Stale[0].ID),
	}
	with := func(edit func([]staleRemovalAttempt) []staleRemovalAttempt) staleRemovalHost {
		return staleRemovalHost{Executor: plan.Executor, Attempts: edit(append([]staleRemovalAttempt(nil), good...))}
	}

	cases := []struct {
		name                  string
		host                  staleRemovalHost
		wantBlocked, wantFail string
	}{
		{name: "controls refused, stale entry removed after a retryable refusal", host: with(func(a []staleRemovalAttempt) []staleRemovalAttempt { return a })},
		{name: "the live voter was removed", wantFail: "was not refused",
			host: with(func(a []staleRemovalAttempt) []staleRemovalAttempt {
				a[1] = staleRemovalAttempt{N: 2, Kind: "control-voter", ID: plan.Voter.ID, Stdout: `{"status":"removed"}`}
				return a
			})},
		{name: "the leader refused for the wrong reason", wantFail: `not refused with "leader"`,
			host: with(func(a []staleRemovalAttempt) []staleRemovalAttempt {
				a[0] = refused(1, "control-leader", plan.Leader.ID, "voter", false)
				return a
			})},
		{name: "the stale entry was refused for good", wantFail: "was not removed",
			host: with(func(a []staleRemovalAttempt) []staleRemovalAttempt {
				return append(a[:2], refused(3, "stale", plan.Stale[0].ID, "reachable", false))
			})},
		{name: "a non-retryable refusal was retried", wantFail: "retried after a non-retryable answer",
			host: with(func(a []staleRemovalAttempt) []staleRemovalAttempt {
				a[2] = refused(3, "stale", plan.Stale[0].ID, "voters_unreachable", false)
				return a
			})},
		{name: "the CLI printed no answer", wantBlocked: "printed no JSON answer",
			host: with(func(a []staleRemovalAttempt) []staleRemovalAttempt {
				a[3] = staleRemovalAttempt{N: 4, Kind: "stale", ID: plan.Stale[0].ID, RC: 1, Stderr: "error: unable to upgrade connection"}
				return a
			})},
		{name: "a control was never run", wantBlocked: "no control-voter attempt",
			host: with(func(a []staleRemovalAttempt) []staleRemovalAttempt { return append(a[:1], a[2:]...) })},
		{name: "the refusal never reached stderr", wantFail: "did not reach stderr",
			host: with(func(a []staleRemovalAttempt) []staleRemovalAttempt {
				a[0].Stderr = ""
				return a
			})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocked, failed := judgeRemovalAttempts(plan, tc.host)
			if tc.wantBlocked == "" {
				require.Empty(t, blocked)
			} else {
				require.Contains(t, blocked, tc.wantBlocked)
			}
			if tc.wantFail == "" {
				require.Empty(t, failed)
			} else {
				require.Contains(t, failed, tc.wantFail)
			}
		})
	}
}
