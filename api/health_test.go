package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/cluster"
	"github.com/stretchr/testify/require"
)

// healthBody is decoded from the wire rather than from the Go structs, so the
// test fails if the JSON field names the console reads ever move.
type healthBody struct {
	Status string `json:"status"`
	Checks struct {
		Database struct {
			Status string `json:"status"`
		} `json:"database"`
		Nodes struct {
			Status string `json:"status"`
			Count  int64  `json:"count"`
		} `json:"nodes"`
		Cluster *struct {
			Status    string `json:"status"`
			Clustered bool   `json:"clustered"`
			Observed  bool   `json:"observed"`
			Quorum    struct {
				Status            string `json:"status"`
				TotalVoters       int    `json:"total_voters"`
				ReachableVoters   int    `json:"reachable_voters"`
				UnreachableVoters int    `json:"unreachable_voters"`
				UnknownVoters     int    `json:"unknown_voters"`
				RequiredVoters    int    `json:"required_voters"`
				Available         bool   `json:"available"`
				Degraded          bool   `json:"degraded"`
				LeaderAddress     string `json:"leader_address"`
			} `json:"quorum"`
			Members []struct {
				Address      string `json:"address"`
				Role         string `json:"role"`
				Leader       bool   `json:"leader"`
				Reachability string `json:"reachability"`
			} `json:"members"`
		} `json:"cluster"`
	} `json:"checks"`
}

func checkerWithView(view cluster.View) healthChecker {
	return healthChecker{
		database:   func() *CheckResult { return &CheckResult{Status: Healthy, LatencyMs: 1} },
		activeRuns: func() *CheckResult { return &CheckResult{Status: Healthy} },
		triggers:   func() *CheckResult { return &CheckResult{Status: Healthy} },
		workers:    func() int64 { return 0 },
		cluster:    func() cluster.View { return view },
		uptime:     func() time.Duration { return time.Minute },
	}
}

func clusterView(members []cluster.Member, leader string) cluster.View {
	return cluster.View{
		Clustered:  true,
		Observed:   true,
		ObservedAt: time.Now().UTC(),
		Members:    members,
		Quorum:     cluster.Summarize(members, leader),
	}
}

func voterMember(addr string, r cluster.Reachability) cluster.Member {
	return cluster.Member{Address: addr, Role: cluster.RoleVoter, Reachability: r}
}

func decodeHealth(t *testing.T, checker healthChecker) (healthBody, int) {
	t.Helper()

	rec := performRequest(t, checker.handle, http.MethodGet, "/health")

	var body healthBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body, rec.Code
}

func TestHealthReportsFullyReachableQuorumAsHealthy(t *testing.T) {
	body, code := decodeHealth(t, checkerWithView(clusterView([]cluster.Member{
		voterMember("10.244.0.8:9001", cluster.Reachable),
		voterMember("10.244.0.9:9001", cluster.Reachable),
		voterMember("10.244.0.10:9001", cluster.Reachable),
	}, "10.244.0.8:9001")))

	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "healthy", body.Status)
	require.NotNil(t, body.Checks.Cluster)
	require.Equal(t, "healthy", body.Checks.Cluster.Status)
	require.Equal(t, "available", body.Checks.Cluster.Quorum.Status)
	require.Equal(t, 3, body.Checks.Cluster.Quorum.ReachableVoters)
	require.Equal(t, 3, body.Checks.Cluster.Quorum.TotalVoters)
	require.True(t, body.Checks.Cluster.Quorum.Available)
	require.False(t, body.Checks.Cluster.Quorum.Degraded)
	require.Equal(t, "healthy", body.Checks.Nodes.Status)
	require.Equal(t, int64(3), body.Checks.Nodes.Count)
}

// Regression for issue #494: a three-replica cluster with one crashed replica
// reported "All systems operational" and quorum 3/3.
func TestHealthReportsCrashedReplicaAsDegradedButAvailable(t *testing.T) {
	body, code := decodeHealth(t, checkerWithView(clusterView([]cluster.Member{
		voterMember("10.244.0.8:9001", cluster.Reachable),
		voterMember("10.244.0.9:9001", cluster.Reachable),
		voterMember("10.244.0.10:9001", cluster.Unreachable),
	}, "10.244.0.8:9001")))

	// The pod must stay live and ready: one dead peer may not fail every
	// replica's Kubernetes probe.
	require.Equal(t, http.StatusOK, code)

	require.Equal(t, "degraded", body.Status)
	require.Equal(t, "degraded", body.Checks.Cluster.Status)
	require.Equal(t, "degraded", body.Checks.Cluster.Quorum.Status)
	require.Equal(t, 2, body.Checks.Cluster.Quorum.ReachableVoters)
	require.Equal(t, 3, body.Checks.Cluster.Quorum.TotalVoters)
	require.Equal(t, 1, body.Checks.Cluster.Quorum.UnreachableVoters)
	require.True(t, body.Checks.Cluster.Quorum.Available, "two of three voters still serve")
	require.True(t, body.Checks.Cluster.Quorum.Degraded)

	// The dead member is still listed — as unreachable, not as evidence of
	// availability.
	require.Len(t, body.Checks.Cluster.Members, 3)
	var dead int
	for _, m := range body.Checks.Cluster.Members {
		if m.Reachability == "unreachable" {
			dead++
			require.Equal(t, "10.244.0.10:9001", m.Address)
		}
	}
	require.Equal(t, 1, dead)

	require.Equal(t, "degraded", body.Checks.Nodes.Status, "the Nodes check must not be unconditionally green")
	require.Equal(t, int64(2), body.Checks.Nodes.Count)
}

func TestHealthReportsLostQuorumAsUnavailableWithoutFailingTheProbe(t *testing.T) {
	body, code := decodeHealth(t, checkerWithView(clusterView([]cluster.Member{
		voterMember("10.244.0.8:9001", cluster.Reachable),
		voterMember("10.244.0.9:9001", cluster.Unreachable),
		voterMember("10.244.0.10:9001", cluster.Unreachable),
	}, "")))

	require.Equal(t, http.StatusOK, code, "a cluster-wide condition must not restart every replica")
	require.Equal(t, "unavailable", body.Status)
	require.Equal(t, "unavailable", body.Checks.Cluster.Status)
	require.Equal(t, "unavailable", body.Checks.Cluster.Quorum.Status)
	require.False(t, body.Checks.Cluster.Quorum.Available)
	require.Equal(t, 1, body.Checks.Cluster.Quorum.ReachableVoters)
	require.Equal(t, 2, body.Checks.Cluster.Quorum.RequiredVoters)
}

func TestHealthNeverReportsUnobservedLivenessAsHealthy(t *testing.T) {
	body, code := decodeHealth(t, checkerWithView(cluster.View{
		Clustered: true,
		Observed:  false,
		Members:   []cluster.Member{},
		Quorum:    cluster.Quorum{Status: cluster.StatusUnknown},
	}))

	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "unknown", body.Status)
	require.Equal(t, "unknown", body.Checks.Cluster.Status)
	require.False(t, body.Checks.Cluster.Observed)
	require.False(t, body.Checks.Cluster.Quorum.Available)
	require.Equal(t, "unknown", body.Checks.Nodes.Status)
}

func TestHealthOmitsClusterCheckWhenNotClustered(t *testing.T) {
	checker := checkerWithView(cluster.View{Clustered: false})
	checker.workers = func() int64 { return 4 }

	body, code := decodeHealth(t, checker)

	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "healthy", body.Status)
	require.Nil(t, body.Checks.Cluster)
	require.Equal(t, "healthy", body.Checks.Nodes.Status)
	require.Equal(t, int64(4), body.Checks.Nodes.Count)
}

func TestHealthStillFailsTheProbeWhenTheLocalDatabaseIsDegraded(t *testing.T) {
	checker := checkerWithView(clusterView([]cluster.Member{
		voterMember("10.244.0.8:9001", cluster.Reachable),
	}, "10.244.0.8:9001"))
	checker.database = func() *CheckResult { return &CheckResult{Status: Degraded, LatencyMs: 2000} }

	body, code := decodeHealth(t, checker)

	require.Equal(t, http.StatusServiceUnavailable, code)
	require.Equal(t, "degraded", body.Status)
	require.Equal(t, "degraded", body.Checks.Database.Status)
}

func TestWorstStatusOrdersTheVocabulary(t *testing.T) {
	require.Equal(t, Degraded, worstStatus(Healthy, Degraded))
	require.Equal(t, Unknown, worstStatus(Degraded, Unknown))
	require.Equal(t, Unavailable, worstStatus(Unknown, Unavailable))
	require.Equal(t, Unavailable, worstStatus(Unavailable, Healthy))
	require.Equal(t, Healthy, worstStatus(Healthy, Healthy))
}
