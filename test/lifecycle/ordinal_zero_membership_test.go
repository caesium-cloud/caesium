package lifecycle

// The ordinal-0 disk-loss membership check is pure so the unit lane can
// exercise it without kind. This file is deliberately untagged: the
// integration-tagged runner (TestLifecycleClusterOrdinalZeroLoss) calls the
// same helper, and its -test.run anchor never selects the table test below.

import (
	"fmt"
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
	sort.Slice(view.nodes, func(i, j int) bool { return view.nodes[i].ID < view.nodes[j].ID })
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
