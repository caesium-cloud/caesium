//go:build integration

package test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// H1 (#582 follow-up): the supported removal of a stale dqlite member,
// `DELETE /v1/system/nodes/:id` and `caesium system nodes list|remove`, driven
// through the live server and the real CLI binary. The integration server is a
// single member, so this scenario proves every refusal that needs no second
// member: an unknown ID, a malformed ID, and the only member (which is also the
// leader, a voter, and the node serving the request). The success path needs a
// lost member and is qualified by F2's ordinal-0/ordinal-1 cases
// (test/lifecycle) and the pkg/dqlite loopback tests.

// raftNodeEntry mirrors a GET /v1/system/nodes entry.
type raftNodeEntry struct {
	ID           string `json:"id"`
	Address      string `json:"address"`
	Role         string `json:"role"`
	Leader       bool   `json:"leader"`
	Reachability string `json:"reachability"`
}

// memberRemovalAnswer is every body DELETE /v1/system/nodes/:id returns.
type memberRemovalAnswer struct {
	Status    string   `json:"status"`
	ID        string   `json:"id"`
	Reason    string   `json:"reason"`
	Reasons   []string `json:"reasons"`
	Message   string   `json:"message"`
	Retryable bool     `json:"retryable"`
	Member    *struct {
		ID      string `json:"id"`
		Address string `json:"address"`
		Role    string `json:"role"`
		Leader  bool   `json:"leader"`
	} `json:"member"`
}

// raftSelf returns the one raft member of the single-node integration server,
// as GET /v1/system/nodes reports it, and checks /health names the same ID.
func (s *IntegrationTestSuite) raftSelf() raftNodeEntry {
	s.T().Helper()

	// The list is served from a liveness snapshot that starts unobserved (seed
	// addresses only, no IDs), so wait for the first observation.
	var nodes, members []raftNodeEntry
	s.Require().Eventually(func() bool {
		nodes, members = nil, nil
		if err := s.tryGetJSON("/v1/system/nodes", &nodes); err != nil {
			return false
		}
		for _, n := range nodes {
			if n.ID != "" {
				members = append(members, n)
			}
		}
		return len(members) > 0
	}, 30*time.Second, 500*time.Millisecond, "GET /v1/system/nodes never listed a raft member with its ID")
	s.Require().Len(members, 1, "the integration server must be a single raft member: %+v", nodes)
	self := members[0]
	s.Require().Equal("voter", self.Role)
	s.Require().True(self.Leader, "a single member leads itself")

	// /health answers 503 whenever this replica cannot serve, so read the body
	// whatever the status: only the reported membership matters here.
	resp, err := s.doRequest(http.MethodGet, s.caesiumURL+"/health", nil)
	s.Require().NoError(err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	var health struct {
		Checks struct {
			Cluster *struct {
				Members []struct {
					ID      uint64 `json:"id"`
					Address string `json:"address"`
				} `json:"members"`
			} `json:"cluster"`
		} `json:"checks"`
	}
	s.Require().NoError(json.Unmarshal(body, &health), string(body))
	s.Require().NotNil(health.Checks.Cluster, "the integration server is backed by dqlite: %s", body)
	s.Require().Len(health.Checks.Cluster.Members, 1, string(body))
	s.Require().Equal(self.ID, strconv.FormatUint(health.Checks.Cluster.Members[0].ID, 10),
		"GET /v1/system/nodes and /health must name the same node ID")
	return self
}

func (s *IntegrationTestSuite) deleteSystemNode(id string) (int, memberRemovalAnswer, string) {
	s.T().Helper()

	resp, err := s.doRequest(http.MethodDelete, s.caesiumURL+"/v1/system/nodes/"+id, nil)
	s.Require().NoError(err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	var answer memberRemovalAnswer
	s.Require().NoError(json.Unmarshal(raw, &answer), "the removal answer must be JSON: %s", raw)
	return resp.StatusCode, answer, string(raw)
}

// requireCLIRefusal runs `caesium system nodes remove <id> --json` and checks
// the refusal contract: a non-zero exit, stdout that is exactly one JSON
// object (captured apart from stderr), and the reason on stderr.
func (s *IntegrationTestSuite) requireCLIRefusal(id, reason string) memberRemovalAnswer {
	s.T().Helper()

	stdout, stderr, err := s.runCLISeparate("system", "nodes", "remove", id, "--json", "--server", s.caesiumURL)
	exitErr, ok := errors.AsType[*exec.ExitError](err)
	s.Require().Truef(ok, "a refused removal must exit non-zero, got %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	s.NotZero(exitErr.ExitCode())
	var answer memberRemovalAnswer
	s.Require().NoError(json.Unmarshal([]byte(stdout), &answer),
		"--json stdout must be exactly the server's JSON answer, with nothing else:\n%s", stdout)
	s.Equal("refused", answer.Status)
	s.Equal(reason, answer.Reason)
	s.Contains(stderr, "refused to remove dqlite member "+id)
	s.Contains(stderr, reason)
	s.NotContains(stdout, "refused to remove", "the human error belongs on stderr")
	return answer
}

func (s *IntegrationTestSuite) TestSystemNodeRemovalRefusals() {
	self := s.raftSelf()

	unknown := "42"
	if self.ID == unknown {
		unknown = "43"
	}

	// REST: every refusal names a precise reason and changes nothing.
	status, answer, raw := s.deleteSystemNode(unknown)
	s.Equal(http.StatusNotFound, status, raw)
	s.Equal("refused", answer.Status)
	s.Equal("not_a_member", answer.Reason)
	s.Equal(unknown, answer.ID)
	s.False(answer.Retryable)

	status, answer, raw = s.deleteSystemNode("not-a-node-id")
	s.Equal(http.StatusBadRequest, status, raw)
	s.Equal("invalid_id", answer.Reason)

	status, answer, raw = s.deleteSystemNode(self.ID)
	s.Equal(http.StatusConflict, status, raw)
	s.Equal("local_node", answer.Reason)
	s.Equal([]string{"local_node", "leader", "voter", "insufficient_voters"}, answer.Reasons,
		"the only member is the serving node, the leader and a voter, and removing it would leave no voters")
	s.False(answer.Retryable)
	s.Require().NotNil(answer.Member)
	s.Equal(self.ID, answer.Member.ID)
	s.Equal("voter", answer.Member.Role)
	s.True(answer.Member.Leader)

	// CLI: the same refusals through the binary.
	answer = s.requireCLIRefusal(self.ID, "local_node")
	s.Equal(self.ID, answer.ID)
	s.requireCLIRefusal(unknown, "not_a_member")

	stdout, stderr, err := s.runCLISeparate("system", "nodes", "remove", "not-a-node-id", "--json", "--server", s.caesiumURL)
	s.Require().Error(err, "a malformed ID must fail")
	s.Empty(stdout, "a malformed ID is rejected before any request, so there is no answer to print")
	s.Contains(stderr, "is not a dqlite node ID")

	// `list` shows the ID an operator passes to `remove`.
	stdout, stderr, err = s.runCLISeparate("system", "nodes", "list", "--server", s.caesiumURL)
	s.Require().NoError(err, stderr)
	var selfRow []string
	for _, line := range strings.Split(stdout, "\n") {
		if fields := strings.Fields(line); len(fields) == 5 && fields[0] == self.ID {
			selfRow = fields
		}
	}
	s.Require().Len(selfRow, 5, "`caesium system nodes list` must print a row for the member's ID:\n%s", stdout)
	s.Equal([]string{self.ID, self.Address, "voter"}, selfRow[:3],
		"`caesium system nodes list` must print the member's ID, address and role:\n%s", stdout)
	s.Contains([]string{"reachable", "unreachable", "unknown"}, selfRow[3])
	s.Equal("true", selfRow[4], "the only member leads")

	stdout, stderr, err = s.runCLISeparate("system", "nodes", "list", "--json", "--server", s.caesiumURL)
	s.Require().NoError(err, stderr)
	var listed []raftNodeEntry
	s.Require().NoError(json.Unmarshal([]byte(stdout), &listed), "list --json stdout must be exactly one JSON array:\n%s", stdout)
	found := false
	for _, n := range listed {
		found = found || (n.ID == self.ID && n.Address == self.Address)
	}
	s.True(found, "list --json must carry the member's ID: %s", stdout)

	// Nothing above changed the membership.
	s.Require().Eventually(func() bool {
		var nodes []raftNodeEntry
		if err := s.tryGetJSON("/v1/system/nodes", &nodes); err != nil {
			return false
		}
		for _, n := range nodes {
			if n.ID == self.ID {
				return n.Role == "voter" && n.Leader
			}
		}
		return false
	}, 30*time.Second, time.Second, "the only member must still be the leading voter")
}

// TestAuthSystemNodeRemovalRequiresAdmin pins the gate on the one route that
// changes raft membership: viewer, runner and operator keys and a job-scoped
// admin key are refused before the handler, while an unscoped admin reaches it
// (and is refused there for an unknown ID). The CLI surfaces the 403.
func (s *IntegrationTestSuite) TestAuthSystemNodeRemovalRequiresAdmin() {
	s.requireAuthLane()

	stamp := time.Now().UnixNano()
	for _, role := range []string{"viewer", "runner", "operator"} {
		key := s.createAPIKeyCLI("--role", role, "--description", fmt.Sprintf("system-nodes-%s-%d", role, stamp))
		status, body := s.requestWithKey(http.MethodDelete, "/v1/system/nodes/42", key.Plaintext, nil)
		s.Equalf(http.StatusForbidden, status, "a %s key must not remove cluster members: %s", role, body)
	}

	scoped := s.createAPIKeyCLI("--role", "admin", "--scope-jobs", fmt.Sprintf("system-nodes-scope-%d", stamp),
		"--description", fmt.Sprintf("system-nodes-scoped-admin-%d", stamp))
	status, body := s.requestWithKey(http.MethodDelete, "/v1/system/nodes/42", scoped.Plaintext, nil)
	s.Equal(http.StatusForbidden, status, "a job-scoped key has no job to authorize cluster membership against: %s", body)

	status, body = s.requestWithKey(http.MethodDelete, "/v1/system/nodes/42", s.authAPIKey, nil)
	s.Require().Equal(http.StatusNotFound, status, "an unscoped admin reaches the handler: %s", body)
	var answer memberRemovalAnswer
	s.Require().NoError(json.Unmarshal([]byte(body), &answer), body)
	s.Equal("not_a_member", answer.Reason)

	operator := s.createAPIKeyCLI("--role", "operator", "--description", fmt.Sprintf("system-nodes-cli-%d", stamp))
	stdout, stderr, err := s.runCLIWithEnv([]string{"CAESIUM_API_KEY=" + operator.Plaintext},
		"system", "nodes", "remove", "42", "--server", s.caesiumURL)
	s.Require().Error(err, "an operator's removal must fail")
	s.Empty(stdout)
	s.Contains(stderr, "HTTP 403")
}
