package dqlite

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	dqliteapp "github.com/canonical/go-dqlite/v3/app"
	"github.com/canonical/go-dqlite/v3/client"
	"github.com/stretchr/testify/require"
)

// startRemovalNode starts a node the way the chart configures one: three
// voters, no standbys, so a joined member stays a spare until role adjustment
// needs it. Roles are adjusted every second instead of every 30.
func startRemovalNode(t *testing.T, ctx context.Context, dir, address string, cluster []string) *dqliteapp.App {
	t.Helper()
	opts := []dqliteapp.Option{
		dqliteapp.WithAddress(address),
		dqliteapp.WithVoters(3),
		dqliteapp.WithStandBys(0),
		dqliteapp.WithRolesAdjustmentFrequency(testRolesAdjustment),
	}
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

// startVoters starts a three-voter cluster and waits until all three vote.
func startVoters(t *testing.T, ctx context.Context, addrs []string) []*dqliteapp.App {
	t.Helper()
	apps := make([]*dqliteapp.App, len(addrs))
	t.Cleanup(func() {
		for _, app := range apps {
			if app != nil {
				_ = app.Close()
			}
		}
	})
	for idx := range addrs {
		apps[idx] = startRemovalNode(t, ctx, t.TempDir(), addrs[idx], addrs[:idx])
	}
	require.Eventually(t, func() bool {
		byID, _, err := leaderMembers(ctx, apps[0])
		if err != nil || len(byID) != len(addrs) {
			return false
		}
		for _, m := range byID {
			if m.Role != client.Voter {
				return false
			}
		}
		return true
	}, 60*time.Second, 250*time.Millisecond, "three-voter cluster never formed")
	return apps
}

func requireRefusal(t *testing.T, err error, reason RemovalReason) *MemberRemovalRefusal {
	t.Helper()
	refusal, ok := errors.AsType[*MemberRemovalRefusal](err)
	require.Truef(t, ok, "want a %s refusal, got %v", reason, err)
	require.Equal(t, reason, refusal.Reason, refusal.Detail)
	return refusal
}

func leaderID(t *testing.T, ctx context.Context, app *dqliteapp.App) uint64 {
	t.Helper()
	cli, err := app.FindLeader(ctx)
	require.NoError(t, err)
	defer func() { _ = cli.Close() }()
	leader, err := cli.Leader(ctx)
	require.NoError(t, err)
	require.NotNil(t, leader)
	return leader.ID
}

// requireEveryViewLacks waits until every live node's own committed
// configuration omits id and holds exactly wantVoters voters.
func requireEveryViewLacks(t *testing.T, ctx context.Context, apps []*dqliteapp.App, id uint64, wantVoters int) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, app := range apps {
			if app == nil {
				continue
			}
			members, err := localMembers(ctx, app)
			if err != nil {
				return false
			}
			voters := 0
			for _, m := range members {
				if m.ID == id {
					return false
				}
				if m.Role == client.Voter {
					voters++
				}
			}
			if voters != wantVoters {
				return false
			}
		}
		return true
	}, 30*time.Second, 250*time.Millisecond, "member %d is still in some node's own configuration", id)
}

// TestRemoveStaleMemberAfterDiskLoss is the #582 follow-up end to end: a voter
// loses its disk, its replacement joins under a fresh ID and is promoted, and
// the lost entry is demoted to a spare that nothing removes. The removal must
// refuse every live member and every identity it must never touch, succeed
// exactly once when the same stale ID is removed through every node at once,
// and leave every node's own configuration without the entry.
func TestRemoveStaleMemberAfterDiskLoss(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	addrs := []string{"127.0.0.1:9551", "127.0.0.1:9552", "127.0.0.1:9553"}
	apps := startVoters(t, ctx, addrs)
	lostID := apps[0].ID()

	// Disk loss: node 0 stops for good and a replacement with an empty data
	// directory joins at a new address through the survivors.
	require.NoError(t, apps[0].Close())
	apps[0] = startRemovalNode(t, ctx, t.TempDir(), "127.0.0.2:9551", addrs[1:])
	fresh := apps[0]
	require.NotEqual(t, lostID, fresh.ID())

	require.Eventually(t, func() bool {
		byID, _, err := leaderMembers(ctx, apps[1])
		if err != nil {
			return false
		}
		self, okSelf := byID[fresh.ID()]
		lost, okLost := byID[lostID]
		return okSelf && okLost && self.Role == client.Voter && lost.Role == client.Spare
	}, 60*time.Second, 250*time.Millisecond, "replacement never promoted or lost voter never demoted")

	// An ID that was never a member.
	_, err := removeStaleMember(ctx, apps[1], 42)
	requireRefusal(t, err, RemovalNotAMember)

	// The node serving the request.
	_, err = removeStaleMember(ctx, apps[1], apps[1].ID())
	refusal := requireRefusal(t, err, RemovalLocalNode)
	require.Contains(t, refusal.Reasons, RemovalVoter)
	require.False(t, refusal.Retryable)

	// The leader, asked through a follower, and a live voter that is neither.
	leader := leaderID(t, ctx, apps[1])
	var follower, otherVoter *dqliteapp.App
	for _, app := range apps {
		if app.ID() == leader {
			continue
		}
		if follower == nil {
			follower = app
		} else {
			otherVoter = app
		}
	}
	require.NotNil(t, otherVoter)
	_, err = removeStaleMember(ctx, follower, leader)
	refusal = requireRefusal(t, err, RemovalLeader)
	require.Equal(t, []RemovalReason{RemovalLeader, RemovalVoter, RemovalInsufficientVoters}, refusal.Reasons)

	_, err = removeStaleMember(ctx, follower, otherVoter.ID())
	refusal = requireRefusal(t, err, RemovalVoter)
	require.Equal(t, []RemovalReason{RemovalVoter, RemovalInsufficientVoters}, refusal.Reasons,
		"removing one of three voters would also leave two")
	require.False(t, refusal.Retryable, "a live voter is never going to be removable")
	require.NotNil(t, refusal.Member)
	require.NotNil(t, refusal.Member.Reachable)
	require.True(t, *refusal.Member.Reachable)

	// Nothing so far changed the configuration.
	byID, _, err := leaderMembers(ctx, apps[1])
	require.NoError(t, err)
	require.Len(t, byID, 4)

	// The same stale ID through every node at once, twice per node: exactly
	// one removal happens; every other caller learns it lost the race.
	const perNode = 2
	start := make(chan struct{})
	results := make([]error, len(apps)*perNode)
	removed := make([]MemberRemoval, len(apps)*perNode)
	var wg sync.WaitGroup
	for idx := range results {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			removed[idx], results[idx] = removeStaleMember(ctx, apps[idx%len(apps)], lostID)
		}(idx)
	}
	close(start)
	wg.Wait()
	successes := 0
	for idx, err := range results {
		if err == nil {
			successes++
			require.Equal(t, lostID, removed[idx].Removed.ID)
			require.Equal(t, "spare", removed[idx].Removed.Role)
			require.Equal(t, addrs[0], removed[idx].Removed.Address)
			require.Len(t, removed[idx].Members, 3)
			continue
		}
		refusal, ok := errors.AsType[*MemberRemovalRefusal](err)
		require.Truef(t, ok, "caller %d: want a refusal, got %v", idx, err)
		require.Containsf(t, []RemovalReason{RemovalNotAMember, RemovalConfigurationChange}, refusal.Reason,
			"caller %d lost the race with an unexpected reason: %s", idx, refusal.Detail)
	}
	require.Equal(t, 1, successes, "exactly one concurrent removal may succeed: %v", results)

	requireEveryViewLacks(t, ctx, apps, lostID, 3)

	// Removing it again is a plain "not a member".
	_, err = removeStaleMember(ctx, apps[2], lostID)
	requireRefusal(t, err, RemovalNotAMember)
}

// TestRemoveStaleMemberRefusesLiveSpareAndConfigurationChange covers the two
// refusals that depend on timing rather than on the configuration alone: a
// spare that still answers, and a removal issued while the leader is applying
// another configuration change. The in-flight change is the promotion of a
// stopped spare: raft holds it open until the catch-up round times out, and
// refuses every other configuration change meanwhile.
func TestRemoveStaleMemberRefusesLiveSpareAndConfigurationChange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	addrs := []string{"127.0.0.1:9561", "127.0.0.1:9562", "127.0.0.1:9563"}
	apps := startVoters(t, ctx, addrs)
	spareE := startRemovalNode(t, ctx, t.TempDir(), "127.0.0.1:9564", addrs)
	spareF := startRemovalNode(t, ctx, t.TempDir(), "127.0.0.1:9565", addrs)
	spareEID, spareFID := spareE.ID(), spareF.ID()
	stopped := false
	defer func() {
		if !stopped {
			_ = spareE.Close()
			_ = spareF.Close()
		}
	}()

	require.Eventually(t, func() bool {
		byID, _, err := leaderMembers(ctx, apps[0])
		return err == nil && len(byID) == 5 &&
			byID[spareEID].Role == client.Spare && byID[spareFID].Role == client.Spare
	}, 60*time.Second, 250*time.Millisecond, "spares never joined")

	// A spare that answers is a member, not a leftover.
	_, err := removeStaleMember(ctx, apps[1], spareEID)
	refusal := requireRefusal(t, err, RemovalReachable)
	require.NotNil(t, refusal.Member)
	require.True(t, *refusal.Member.Reachable)
	require.Equal(t, "spare", refusal.Member.Role)

	require.NoError(t, spareE.Close())
	require.NoError(t, spareF.Close())
	stopped = true

	// Promote the stopped spare F: the leader starts a catch-up round F never
	// answers, and keeps the change open until the round times out.
	cli, err := apps[1].FindLeader(ctx)
	require.NoError(t, err)
	defer func() { _ = cli.Close() }()
	assigned := make(chan error, 1)
	promotionStarted := time.Now()
	go func() {
		assignCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		assigned <- cli.Assign(assignCtx, spareFID, client.Voter)
	}()
	time.Sleep(300 * time.Millisecond)

	_, err = removeStaleMember(ctx, apps[1], spareEID)
	refusal = requireRefusal(t, err, RemovalConfigurationChange)
	require.True(t, refusal.Retryable)
	byID, _, err := leaderMembers(ctx, apps[1])
	require.NoError(t, err)
	require.Equal(t, client.Spare, byID[spareEID].Role, "a refused removal must leave the member in place")

	select {
	case err := <-assigned:
		require.Error(t, err, "a stopped spare cannot complete its promotion")
		// Measured at about 50 s on loopback: how long a stalled promotion can
		// hold every other configuration change off.
		t.Logf("the stalled promotion ended after %s: %v", time.Since(promotionStarted).Round(time.Second), err)
	case <-ctx.Done():
		t.Fatal("the in-flight promotion never ended")
	}

	// Once the promotion is abandoned, the same request succeeds: that is the
	// documented retry.
	var result MemberRemoval
	require.Eventually(t, func() bool {
		result, err = removeStaleMember(ctx, apps[1], spareEID)
		return err == nil
	}, 60*time.Second, 500*time.Millisecond, "removal never succeeded after the configuration change ended: %v", err)
	require.Equal(t, spareEID, result.Removed.ID)

	_, err = removeStaleMember(ctx, apps[2], spareFID)
	require.NoError(t, err)

	requireEveryViewLacks(t, ctx, apps, spareEID, 3)
	requireEveryViewLacks(t, ctx, apps, spareFID, 3)
}

func TestRemovalRefusalsDecisionTable(t *testing.T) {
	const local, leader, other, stale = 1, 2, 3, 4
	voters := func(ids ...uint64) []client.NodeInfo {
		out := make([]client.NodeInfo, 0, len(ids))
		for _, id := range ids {
			out = append(out, client.NodeInfo{ID: id, Address: fmt.Sprintf("10.0.0.%d:9001", id), Role: client.Voter})
		}
		return out
	}
	spare := client.NodeInfo{ID: stale, Address: "10.0.0.4:9001", Role: client.Spare}
	standby := client.NodeInfo{ID: stale, Address: "10.0.0.4:9001", Role: client.StandBy}
	three := voters(local, leader, other)

	cases := []struct {
		name    string
		target  client.NodeInfo
		members []client.NodeInfo
		want    []RemovalReason
	}{
		{"stale spare beside three voters", spare, append(three, spare), nil},
		{"stale standby beside three voters", standby, append(three, standby), nil},
		{"stale voter not yet demoted", client.NodeInfo{ID: stale, Address: "10.0.0.4:9001", Role: client.Voter},
			voters(local, leader, other, stale), []RemovalReason{RemovalVoter}},
		{"the serving node", three[0], three, []RemovalReason{RemovalLocalNode, RemovalVoter, RemovalInsufficientVoters}},
		{"the leader", three[1], three, []RemovalReason{RemovalLeader, RemovalVoter, RemovalInsufficientVoters}},
		{"a single-node cluster removing itself", client.NodeInfo{ID: local, Role: client.Voter},
			[]client.NodeInfo{{ID: local, Role: client.Voter}},
			[]RemovalReason{RemovalLocalNode, RemovalLeader, RemovalVoter, RemovalInsufficientVoters}},
		{"a spare beside only two voters", spare, append(voters(local, leader), spare),
			[]RemovalReason{RemovalInsufficientVoters}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			leaderOf := uint64(leader)
			if len(tc.members) == 1 {
				leaderOf = local
			}
			require.Equal(t, tc.want, removalRefusals(tc.target, local, leaderOf, tc.members))
		})
	}
}

func TestRemoveStaleMemberWithoutANodeIsNotClustered(t *testing.T) {
	require.Nil(t, currentApp.Load())
	_, err := RemoveStaleMember(context.Background(), 42)
	refusal := requireRefusal(t, err, RemovalNotClustered)
	require.False(t, refusal.Retryable)
}
