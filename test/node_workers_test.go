//go:build integration

package test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Stream C6 — GET /v1/nodes/:address/workers, the per-node worker-claim view
// behind the UI's node drawer. It had no integration coverage at all, so
// nothing pinned the response shape or the fact that the :address segment
// carries a host:port (a colon inside one path segment) rather than an id.
// The RBAC policy key for the route is the normalised
// "GET /v1/nodes/:id/workers" (internal/auth/rbac.go).

type nodeSummary struct {
	Address      string `json:"address"`
	Role         string `json:"role"`
	WorkersBusy  int    `json:"workers_busy"`
	WorkersTotal int    `json:"workers_total"`
}

type workerStatusView struct {
	Address            string           `json:"address"`
	ObservedAt         time.Time        `json:"observed_at"`
	TotalClaimedTasks  int64            `json:"total_claimed_tasks"`
	ClaimedByStatus    map[string]int64 `json:"claimed_by_status"`
	RunningClaims      int64            `json:"running_claims"`
	ExpiredLeases      int64            `json:"expired_leases"`
	TotalClaimAttempts int64            `json:"total_claim_attempts"`
	ActiveClaims       []struct {
		JobRunID string `json:"job_run_id"`
		TaskID   string `json:"task_id"`
		Status   string `json:"status"`
	} `json:"active_claims"`
}

func (s *IntegrationTestSuite) TestNodeWorkersRoute() {
	var nodes []nodeSummary
	s.getJSON("/v1/system/nodes", &nodes)
	s.Require().NotEmpty(nodes, "the live server must report at least its own node")

	address := nodes[0].Address
	s.Require().NotEmpty(address, "a node entry must carry an address")

	status, body := s.workerStatusRequest(address)
	s.Require().Equalf(http.StatusOK, status,
		"GET /v1/nodes/%s/workers must be a bound route: %s", address, body)

	var view workerStatusView
	s.Require().NoError(json.Unmarshal([]byte(body), &view), body)
	s.Equal(address, view.Address, "the response must echo the requested node address")
	s.False(view.ObservedAt.IsZero(), "observed_at must be stamped")
	// Both collections are initialised server-side, so a client never has to
	// distinguish "no claims" from "field missing".
	s.NotNil(view.ClaimedByStatus, "claimed_by_status must be an object, not null")
	s.NotNil(view.ActiveClaims, "active_claims must be an array, not null")
	s.GreaterOrEqual(view.TotalClaimedTasks, int64(0))

	// A node that has never claimed anything is an empty report, not an error:
	// the handler answers for any address (api/rest/service/worker/worker.go
	// Status), which is what makes the UI's node drawer safe to open.
	status, body = s.workerStatusRequest("203.0.113.9:9999")
	s.Require().Equal(http.StatusOK, status, body)
	s.Require().NoError(json.Unmarshal([]byte(body), &view), body)
	s.Equal("203.0.113.9:9999", view.Address)
	s.Equal(int64(0), view.TotalClaimedTasks)
	s.Empty(view.ActiveClaims)
}

func (s *IntegrationTestSuite) workerStatusRequest(address string) (int, string) {
	s.T().Helper()

	target := s.caesiumURL + "/v1/nodes/" + url.PathEscape(address) + "/workers"
	resp, err := s.doRequest(http.MethodGet, target, nil)
	s.Require().NoError(err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	return resp.StatusCode, string(body)
}
