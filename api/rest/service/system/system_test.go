package system

import (
	"context"
	"testing"
	"time"

	clustersvc "github.com/caesium-cloud/caesium/internal/cluster"
	"github.com/stretchr/testify/require"
)

func clusteredView(members ...clustersvc.Member) clustersvc.View {
	return clustersvc.View{
		Clustered:  true,
		Observed:   true,
		ObservedAt: time.Now().UTC(),
		Members:    members,
		Quorum:     clustersvc.Summarize(members, members[0].Address),
		Nodes:      clustersvc.SummarizeNodes(members),
	}
}

func voter(addr string, r clustersvc.Reachability, leader bool) clustersvc.Member {
	return clustersvc.Member{Address: addr, Role: clustersvc.RoleVoter, Leader: leader, Reachability: r}
}

// TestNodesReturnsWithinTheBoundWhenTheDatabaseBlocks is the regression for the
// review finding: the enrichment queries route through dqlite, whose driver
// retries leader discovery until its context is done. Issued without a deadline
// they block for as long as the cluster has no leader — precisely the outage
// this endpoint exists to describe.
func TestNodesReturnsWithinTheBoundWhenTheDatabaseBlocks(t *testing.T) {
	members := []clustersvc.Member{
		voter("10.244.0.8:9001", clustersvc.Reachable, true),
		voter("10.244.0.9:9001", clustersvc.Unreachable, false),
		voter("10.244.0.10:9001", clustersvc.Unreachable, false),
	}

	svc := &Service{
		ctx:      context.Background(),
		snapshot: func() clustersvc.View { return clusteredView(members...) },
		enrich: func(ctx context.Context) (nodeUsage, error) {
			<-ctx.Done() // the dqlite driver's unbounded leader search
			return nodeUsage{}, ctx.Err()
		},
		timeout: 150 * time.Millisecond,
	}

	start := time.Now()
	nodes, err := svc.Nodes()
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.Less(t, elapsed, 5*time.Second, "the endpoint hung instead of bounding the database work")

	// Membership and liveness — the point of the endpoint — are still served.
	byAddr := map[string]Node{}
	for _, n := range nodes {
		byAddr[n.Address] = n
	}
	for _, m := range members {
		require.Contains(t, byAddr, m.Address)
		require.Equal(t, m.Reachability, byAddr[m.Address].Reachability)
		require.Equal(t, m.Leader, byAddr[m.Address].Leader)
	}
	require.True(t, byAddr["10.244.0.8:9001"].Leader)
	require.Equal(t, clustersvc.Unreachable, byAddr["10.244.0.10:9001"].Reachability)

	// The worker counts are not known, and must not be reported as zero.
	for _, n := range nodes {
		require.Nil(t, n.WorkersBusy, "node %s reported a busy count the database never returned", n.Address)
	}
}

func TestNodesReportsWorkerCountsWhenTheDatabaseAnswers(t *testing.T) {
	members := []clustersvc.Member{
		voter("10.244.0.8:9001", clustersvc.Reachable, true),
		voter("10.244.0.9:9001", clustersvc.Reachable, false),
	}

	svc := &Service{
		ctx:      context.Background(),
		snapshot: func() clustersvc.View { return clusteredView(members...) },
		enrich: func(context.Context) (nodeUsage, error) {
			return nodeUsage{
				workers:    map[string]int{"10.244.0.8:9001": 2},
				historical: []string{"10.244.0.8:9001", "10.0.0.99:9001"},
			}, nil
		},
		timeout: time.Second,
	}

	nodes, err := svc.Nodes()
	require.NoError(t, err)

	byAddr := map[string]Node{}
	for _, n := range nodes {
		byAddr[n.Address] = n
	}

	require.Contains(t, byAddr, "10.244.0.8:9001")
	require.NotNil(t, byAddr["10.244.0.8:9001"].WorkersBusy)
	require.Equal(t, 2, *byAddr["10.244.0.8:9001"].WorkersBusy)

	// A member with no running tasks reports zero, not unknown.
	require.NotNil(t, byAddr["10.244.0.9:9001"].WorkersBusy)
	require.Equal(t, 0, *byAddr["10.244.0.9:9001"].WorkersBusy)

	// A historical worker that is not a raft member is listed with unknown
	// liveness — it was never probed.
	require.Contains(t, byAddr, "10.0.0.99:9001")
	require.Equal(t, RoleWorker, byAddr["10.0.0.99:9001"].Role)
	require.Equal(t, clustersvc.Unknown, byAddr["10.0.0.99:9001"].Reachability)
}

func TestNodesDegradesWhenEnrichmentFails(t *testing.T) {
	members := []clustersvc.Member{voter("10.244.0.8:9001", clustersvc.Reachable, true)}

	svc := &Service{
		ctx:      context.Background(),
		snapshot: func() clustersvc.View { return clusteredView(members...) },
		enrich: func(context.Context) (nodeUsage, error) {
			return nodeUsage{}, context.DeadlineExceeded
		},
		timeout: time.Second,
	}

	nodes, err := svc.Nodes()

	require.NoError(t, err, "a failed enrichment must not fail the endpoint")
	require.Len(t, nodes, 1)
	require.Nil(t, nodes[0].WorkersBusy)
	require.Equal(t, clustersvc.Reachable, nodes[0].Reachability)
}
