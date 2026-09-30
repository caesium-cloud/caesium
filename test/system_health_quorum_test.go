//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type healthQuorum struct {
	Status            string `json:"status"`
	TotalVoters       int    `json:"total_voters"`
	ReachableVoters   int    `json:"reachable_voters"`
	UnreachableVoters int    `json:"unreachable_voters"`
	UnknownVoters     int    `json:"unknown_voters"`
	RequiredVoters    int    `json:"required_voters"`
	Available         bool   `json:"available"`
	Degraded          bool   `json:"degraded"`
	LeaderAddress     string `json:"leader_address"`
}

type healthClusterMember struct {
	Address      string `json:"address"`
	Role         string `json:"role"`
	Leader       bool   `json:"leader"`
	Reachability string `json:"reachability"`
	LatencyMs    *int64 `json:"latency_ms"`
}

type healthClusterCheck struct {
	Status     string                `json:"status"`
	Clustered  bool                  `json:"clustered"`
	Observed   bool                  `json:"observed"`
	ObservedAt string                `json:"observed_at"`
	Quorum     healthQuorum          `json:"quorum"`
	Members    []healthClusterMember `json:"members"`
}

type healthEnvelope struct {
	Status string `json:"status"`
	Uptime int64  `json:"uptime"`
	Checks struct {
		Database struct {
			Status string `json:"status"`
		} `json:"database"`
		Nodes struct {
			Status string `json:"status"`
			Count  int64  `json:"count"`
		} `json:"nodes"`
		Cluster *healthClusterCheck `json:"cluster"`
	} `json:"checks"`
}

type systemNode struct {
	Address      string `json:"address"`
	Role         string `json:"role"`
	Leader       bool   `json:"leader"`
	Reachability string `json:"reachability"`
	LatencyMs    *int64 `json:"latency_ms"`
	WorkersBusy  *int   `json:"workers_busy"`
	WorkersTotal int    `json:"workers_total"`
}

func (s *IntegrationTestSuite) fetchHealth() (healthEnvelope, int) {
	resp, err := s.doRequest(http.MethodGet, s.caesiumURL+"/health", nil)
	require.NoError(s.T(), err)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(s.T(), err)

	var envelope healthEnvelope
	require.NoError(s.T(), json.Unmarshal(body, &envelope), string(body))
	return envelope, resp.StatusCode
}

// TestSystemHealthReportsProbedQuorum drives the real `/health` surface and
// asserts the cluster liveness contract the console reads. Before issue #494
// the response carried no liveness at all: the console derived "quorum 3/3"
// from the length of the tracked node list, so a replica that had been dead for
// minutes still rendered as evidence of availability.
//
// This lane runs a single-node dqlite cluster, so the healthy end of the
// contract is what it can observe for real: the voter count, the majority
// threshold, and — crucially — that reachability comes from an ACTUAL dqlite
// RPC (a measured latency) rather than from the node being listed. The
// degraded, quorum-lost and unknown branches are pinned by the decision-table
// tests in internal/cluster and by the handler tests in api/health_test.go.
func (s *IntegrationTestSuite) TestSystemHealthReportsProbedQuorum() {
	var envelope healthEnvelope
	var code int

	// The first observation is taken in the background, so liveness is
	// legitimately unknown for a moment after boot. That it converges is itself
	// part of the contract: an unobserved cluster must never report healthy.
	require.Eventually(s.T(), func() bool {
		envelope, code = s.fetchHealth()
		return envelope.Checks.Cluster != nil && envelope.Checks.Cluster.Observed
	}, 30*time.Second, time.Second, "cluster liveness was never observed")

	require.Equal(s.T(), http.StatusOK, code)
	require.Equal(s.T(), "healthy", envelope.Status)

	check := envelope.Checks.Cluster
	if raw, err := json.Marshal(check); err == nil {
		s.T().Logf("GET /health checks.cluster: %s", raw)
	}
	require.NotNil(s.T(), check, "/health must report a cluster check on a dqlite deployment")
	assert.True(s.T(), check.Clustered)
	assert.Equal(s.T(), "healthy", check.Status)
	assert.NotEmpty(s.T(), check.ObservedAt)

	quorum := check.Quorum
	assert.Equal(s.T(), "available", quorum.Status)
	assert.True(s.T(), quorum.Available)
	assert.False(s.T(), quorum.Degraded)
	assert.Positive(s.T(), quorum.TotalVoters, "membership must be reported")
	assert.Equal(s.T(), quorum.TotalVoters, quorum.ReachableVoters,
		"every voter on a healthy lane must have answered a probe")
	assert.Zero(s.T(), quorum.UnreachableVoters)
	assert.Zero(s.T(), quorum.UnknownVoters)
	assert.Equal(s.T(), quorum.TotalVoters/2+1, quorum.RequiredVoters,
		"required voters must be a strict majority, not the membership count")
	assert.NotEmpty(s.T(), quorum.LeaderAddress)

	require.NotEmpty(s.T(), check.Members, "members must be enumerated")
	leaders := 0
	for _, m := range check.Members {
		assert.NotEmpty(s.T(), m.Address)
		assert.Equal(s.T(), "voter", m.Role)
		assert.Equal(s.T(), "reachable", m.Reachability)
		// latency_ms is only populated by a successful dqlite RPC, so its
		// presence is the evidence that reachability was measured rather than
		// inferred from membership.
		require.NotNil(s.T(), m.LatencyMs, "member %s reported reachable without a measured probe", m.Address)
		if m.Leader {
			leaders++
			assert.Equal(s.T(), quorum.LeaderAddress, m.Address)
		}
	}
	assert.Equal(s.T(), 1, leaders, "exactly one member must be flagged as leader")

	// The Nodes check used to carry no status at all, which the console
	// rendered as unconditionally green.
	assert.Equal(s.T(), "healthy", envelope.Checks.Nodes.Status)
	assert.Equal(s.T(), int64(quorum.ReachableVoters), envelope.Checks.Nodes.Count)
}

// TestHealthProbeEndpointsAreSplit drives the two Kubernetes probe targets the
// Helm chart points at (helm/caesium/templates/statefulset.yaml). They answer
// different questions on purpose: readiness is "can this replica serve?", which
// must fail and pull an unserviceable replica out of the Service endpoints;
// liveness is "is this process running?", which must not restart-loop a replica
// that is merely cut off from its peers.
func (s *IntegrationTestSuite) TestHealthProbeEndpointsAreSplit() {
	// Liveness: minimal, touches no dependency, and is reachable without auth
	// so the kubelet can call it.
	resp, err := s.doRequest(http.MethodGet, s.caesiumURL+"/health/live", nil)
	require.NoError(s.T(), err)
	live, err := io.ReadAll(resp.Body)
	require.NoError(s.T(), resp.Body.Close())
	require.NoError(s.T(), err)

	require.Equal(s.T(), http.StatusOK, resp.StatusCode, string(live))
	var liveness struct {
		Status string `json:"status"`
		Uptime int64  `json:"uptime"`
	}
	require.NoError(s.T(), json.Unmarshal(live, &liveness), string(live))
	assert.Equal(s.T(), "healthy", liveness.Status)
	assert.Positive(s.T(), liveness.Uptime)
	// Liveness must stay cheap: no cluster or dependency detail belongs here.
	assert.NotContains(s.T(), string(live), "checks")

	// Readiness: the full report, and on a healthy lane a serviceable replica.
	readyResp, err := s.doRequest(http.MethodGet, s.caesiumURL+"/health/ready", nil)
	require.NoError(s.T(), err)
	ready, err := io.ReadAll(readyResp.Body)
	require.NoError(s.T(), readyResp.Body.Close())
	require.NoError(s.T(), err)

	require.Equal(s.T(), http.StatusOK, readyResp.StatusCode, string(ready))
	var envelope healthEnvelope
	require.NoError(s.T(), json.Unmarshal(ready, &envelope), string(ready))
	assert.Equal(s.T(), "healthy", envelope.Checks.Database.Status)
	assert.NotNil(s.T(), envelope.Checks.Cluster, "readiness must carry the cluster assessment")
}

// TestSystemNodesReportObservedReachability drives `/v1/system/nodes`, the
// other surface the console's cluster table reads. Every row must carry its
// observed liveness; the raft members must be flagged reachable with exactly
// one leader.
func (s *IntegrationTestSuite) TestSystemNodesReportObservedReachability() {
	health, _ := s.fetchHealth()
	require.NotNil(s.T(), health.Checks.Cluster, "lane is not dqlite-backed")

	var nodes []systemNode
	require.Eventually(s.T(), func() bool {
		nodes = nil
		if err := s.tryGetJSON("/v1/system/nodes", &nodes); err != nil {
			return false
		}
		for _, n := range nodes {
			if n.Reachability == "reachable" {
				return true
			}
		}
		return false
	}, 30*time.Second, time.Second, fmt.Sprintf("no node reported observed reachability: %+v", nodes))

	require.NotEmpty(s.T(), nodes)
	if raw, err := json.Marshal(nodes); err == nil {
		s.T().Logf("GET /v1/system/nodes: %s", raw)
	}

	leaders := 0
	reachable := 0
	for _, n := range nodes {
		assert.NotEmpty(s.T(), n.Address)
		assert.Contains(s.T(), []string{"reachable", "unreachable", "unknown"}, n.Reachability,
			"node %s must report an explicit reachability", n.Address)
		if n.Leader {
			leaders++
			assert.Equal(s.T(), "voter", n.Role, "the leader must be a voter")
			assert.Equal(s.T(), "reachable", n.Reachability, "the leader answered the request, so it cannot be unreachable")
		}
		if n.Reachability == "reachable" {
			reachable++
			require.NotNil(s.T(), n.LatencyMs, "node %s reported reachable without a measured probe", n.Address)
			// The database enrichment is bounded and degrades to null on
			// timeout; on a healthy lane it must actually have completed.
			require.NotNil(s.T(), n.WorkersBusy, "node %s reported no worker count on a healthy lane", n.Address)
		}
	}
	assert.Equal(s.T(), 1, leaders, "exactly one node must be flagged as leader")
	assert.Positive(s.T(), reachable)
}
