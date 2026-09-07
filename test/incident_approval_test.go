//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/pkg/env"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
)

// incident_approval_test.go drives the tier-3 approval pipeline end to end on
// the auth-enabled remediation lane (trust-the-substrate C3 + C8):
//
//	real failing run → incident → agent proposes a tier-3 action through
//	POST /v1/agent/incidents/:id/actions with a real agent-session token →
//	ApprovalRequest exists and the incident parks in awaiting_approval →
//	`caesium incident approve` → THE ACTION ACTUALLY RUNS → `caesium why`
//	names the decider.
//
// The "actually runs" assertion is the point. The pipeline previously recorded a
// proposal nothing could approve and, on the far end, marked an action approved
// with nothing to execute it — a shape that passes any test asserting only on
// statuses. Every scenario here therefore asserts an OBSERVABLE effect on the
// run, not just a row transition.

// approvalIncident is the subset of GET /v1/incidents rows these scenarios read.
type approvalIncident struct {
	ID       string `json:"id"`
	JobID    string `json:"job_id"`
	Status   string `json:"status"`
	TaskName string `json:"task_name"`
	Class    string `json:"class"`
}

type approvalIncidentList struct {
	Incidents []approvalIncident `json:"incidents"`
	Total     int                `json:"total"`
}

type approvalAction struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Tier   int             `json:"tier"`
	Status string          `json:"status"`
	Result json.RawMessage `json:"result"`
}

type approvalRequest struct {
	ID       string `json:"id"`
	ActionID string `json:"action_id"`
	Decision string `json:"decision"`
	Decider  string `json:"decider"`
}

type approvalDetail struct {
	Incident  approvalIncident  `json:"incident"`
	Actions   []approvalAction  `json:"actions"`
	Approvals []approvalRequest `json:"approvals"`
}

// failingJobDefinition is a single-step job whose only task fails. One step keeps
// the assertions unambiguous: the run has exactly one task row, so "the task is
// skipped" needs no name-to-id lookup through an endpoint with its own quirks.
//
// The failure text is deliberately neutral — the deterministic classifier maps
// "not found"/"connection refused"/401 log tails onto classes that carry an
// auto-retry rule, and a retry firing underneath would race these assertions.
// A bare exit 1 classifies as `unknown`, which has no deterministic rule, so the
// incident waits for a proposal exactly as the scenario needs.
//
// The cron never fires (31 February), so the only run is the one this scenario
// triggers.
func failingJobDefinition(alias, engine string) schema.Definition {
	step := schema.Step{
		Name:    "gate",
		Image:   "busybox:1.36.1",
		Command: []string{"sh", "-c", "echo caesium approval lane gate step; exit 1"},
	}
	if engine != "" && engine != "docker" {
		step.Engine = engine
	}
	return schema.Definition{
		APIVersion: "v1",
		Kind:       "Job",
		Metadata:   schema.Metadata{Alias: alias},
		Trigger: schema.Trigger{
			Type:          "cron",
			Configuration: map[string]any{"cron": "0 0 31 2 *"},
		},
		Steps: []schema.Step{step},
	}
}

// applyDefinition applies a job definition through the AUTHENTICATED REST
// surface.
//
// `caesium job apply` is deliberately not used here: it sends no Authorization
// header at all (cmd/job/apply.go `sendApplyRequest`), so it cannot reach an
// auth-enabled server — a real gap, but not this scenario's subject. The CLI is
// still driven for everything this stream ships: `caesium incident
// approve/reject` and `caesium why`.
func (s *IntegrationTestSuite) applyDefinition(def schema.Definition) {
	s.T().Helper()

	payload, err := json.Marshal(map[string]any{"definitions": []schema.Definition{def}})
	s.Require().NoError(err)

	// doJSONRequest, not doRequest: the latter sets no Content-Type, and echo's
	// binder refuses a body it cannot type ("bad request") before the handler
	// ever sees the definitions.
	resp, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/jobdefs/apply", bytes.NewReader(payload))
	s.Require().NoError(err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, "apply %s failed: %s", def.Metadata.Alias, string(body))
}

// TestIncidentApprovalDecisionsCLI is C3: approve and reject, both through a
// REAL agent proposal, with the approved action's effect asserted on the run.
func (s *IntegrationTestSuite) TestIncidentApprovalDecisionsCLI() {
	s.requireAuthLane()

	// --- Approve: the proposed skip_task must actually skip the task ---------
	approved := s.driveTier3SkipTaskProposal("approval-approve")

	// The agent's own token may NEVER approve its own proposal. This is the
	// load-bearing half of "tier 3 always terminates at a human": without it, the
	// agent container — which has network reach to the API — could approve itself.
	status, body := s.postWithToken(
		fmt.Sprintf("%s/v1/incidents/%s/approvals/%s/approve", s.caesiumURL, approved.incidentID, approved.approvalID),
		approved.agentToken, nil)
	s.Require().Equal(http.StatusForbidden, status, string(body))

	// Which of the two denials fires is an ordering fact worth stating rather
	// than asserting blindly. The auth middleware checks the RBAC role BEFORE
	// authorizeScope (api/middleware/auth.go: `auth.HasRole` then
	// `authorizeScope`), and an agent-session key is minted RoleRunner while the
	// approve route requires RoleOperator — so on the wire the role gate answers
	// first with "insufficient permissions", and authorizeScope's specific
	// ApprovalAgentTokenDenyMessage arm is defence in depth that only fires if an
	// agent key ever carried an operator role. (api/middleware/auth_scope_approval_test.go
	// pins that arm by calling authorizeScope directly, so it passes without ever
	// proving the wire behaviour — which is why this assertion accepts either.)
	denied := string(body)
	s.Require().True(
		strings.Contains(denied, authmw.ApprovalAgentTokenDenyMessage) || strings.Contains(denied, "insufficient permissions"),
		"an agent token must be refused the approve route with a documented denial, got: %s", denied)

	// The security property that actually matters: the attempt changed nothing.
	stillPending := s.incidentDetail(approved.incidentID)
	s.Require().Equal("awaiting_approval", stillPending.Incident.Status,
		"an agent's attempt to approve its own action must not advance the incident")
	for _, a := range stillPending.Approvals {
		if a.ID == approved.approvalID {
			s.Require().Equal("pending", a.Decision,
				"an agent's attempt to approve its own action must leave the approval pending")
		}
	}

	out, err := s.runCLIStdout(s.incidentCLIArgs("approve", approved.incidentID,
		"--approval", approved.approvalID, "--reason", "vendor confirmed the file is empty today")...)
	s.Require().NoError(err, "caesium incident approve failed:\n%s", out)
	s.Require().True(json.Valid([]byte(out)),
		"caesium incident approve stdout was not clean JSON (a log line or cobra Print* leaked to stdout):\n%s", out)

	var decided approvalRequest
	s.Require().NoError(json.Unmarshal([]byte(out), &decided))
	s.Require().Equal("approved", decided.Decision)
	s.Require().NotEmpty(decided.Decider, "the approval must record WHO decided")

	// The action EXECUTED — not merely "approved". This is the assertion the
	// pipeline used to fail silently.
	detail := s.incidentDetail(approved.incidentID)
	action := requireActionByID(s, detail, approved.actionID)
	s.Require().Equal("executed", action.Status,
		"an approved tier-3 action must execute, not sit at `approved`")
	var result map[string]any
	s.Require().NoError(json.Unmarshal(action.Result, &result))
	s.Require().Equal(decided.Decider, result["approved_by"])
	s.Require().NotEmpty(result["executed_at"])

	// …and the effect is visible on the run: the failed task is now skipped and
	// the run is terminal.
	run := s.awaitTaskStatus(approved.jobID, approved.runID, "skipped", 30*time.Second)
	s.Require().Contains([]string{"failed", "succeeded"}, run.Status,
		"the run must remain terminal after the approved skip")

	// --- Reject: a second incident, decided the other way --------------------
	rejected := s.driveTier3SkipTaskProposal("approval-reject")

	out, err = s.runCLIStdout(s.incidentCLIArgs("reject", rejected.incidentID,
		"--approval", rejected.approvalID, "--reason", "the data really is missing")...)
	s.Require().NoError(err, "caesium incident reject failed:\n%s", out)
	s.Require().True(json.Valid([]byte(out)),
		"caesium incident reject stdout was not clean JSON:\n%s", out)

	var refused approvalRequest
	s.Require().NoError(json.Unmarshal([]byte(out), &refused))
	s.Require().Equal("rejected", refused.Decision)

	rejectedDetail := s.incidentDetail(rejected.incidentID)
	rejectedAction := requireActionByID(s, rejectedDetail, rejected.actionID)
	s.Require().Equal("rejected", rejectedAction.Status)
	s.Require().Equal("escalated", rejectedDetail.Incident.Status,
		"a rejection must escalate (a human owns it), never resume triaging")
}

// TestIncidentApprovalWhyExplains is C8: the approved remediation is
// EXPLAINABLE. An operator looking at a task that says "skipped" must be able to
// ask `caesium why` and be told who approved the skip and when it ran, rather
// than having to know an incident timeline exists.
func (s *IntegrationTestSuite) TestIncidentApprovalWhyExplains() {
	s.requireAuthLane()

	approved := s.driveTier3SkipTaskProposal("approval-why")

	out, err := s.runCLIStdout(s.incidentCLIArgs("approve", approved.incidentID,
		"--approval", approved.approvalID, "--reason", "approved for the why explainer")...)
	s.Require().NoError(err, "caesium incident approve failed:\n%s", out)

	var decided approvalRequest
	s.Require().NoError(json.Unmarshal([]byte(out), &decided))
	s.Require().Equal("approved", decided.Decision)
	s.Require().NotEmpty(decided.Decider)

	s.awaitTaskStatus(approved.jobID, approved.runID, "skipped", 30*time.Second)

	// Table form: stdout captured SEPARATELY from stderr so a log line leaking to
	// the wrong stream cannot make this pass (CLAUDE.md's machine-output gate).
	args := []string{"why", approved.runID, "--task", "gate", "--job-id", approved.jobID, "--server", s.caesiumURL}
	if key := firstEnv("CAESIUM_API_KEY", "CAESIUM_AUTH_ADMIN_KEY"); key != "" {
		args = append(args, "--api-key", key)
	}
	table, err := s.runCLIStdout(args...)
	s.Require().NoError(err, "caesium why failed:\n%s", table)
	s.Require().Contains(table, "skip_task",
		"caesium why must name the approved action that changed this task's outcome:\n%s", table)
	s.Require().Contains(table, decided.Decider,
		"caesium why must name the decider who approved the skip:\n%s", table)
	s.Require().Contains(table, "Approved remediation",
		"caesium why must render the remediation provenance block:\n%s", table)

	// JSON form: the same provenance, machine-readable.
	jsonOut, err := s.runCLIStdout(append(args, "--json")...)
	s.Require().NoError(err, "caesium why --json failed:\n%s", jsonOut)
	s.Require().True(json.Valid([]byte(jsonOut)), "caesium why --json stdout was not clean JSON:\n%s", jsonOut)

	var explanation struct {
		Remediation []struct {
			Type       string `json:"type"`
			ApprovedBy string `json:"approvedBy"`
			ExecutedAt string `json:"executedAt"`
			Scope      string `json:"scope"`
		} `json:"remediation"`
	}
	s.Require().NoError(json.Unmarshal([]byte(jsonOut), &explanation))
	s.Require().Len(explanation.Remediation, 1)
	s.Require().Equal("skip_task", explanation.Remediation[0].Type)
	s.Require().Equal(decided.Decider, explanation.Remediation[0].ApprovedBy)
	s.Require().NotEmpty(explanation.Remediation[0].ExecutedAt)
	s.Require().Equal("task", explanation.Remediation[0].Scope)
}

// tier3Proposal is everything a decided approval needs to be driven and asserted.
type tier3Proposal struct {
	jobID      string
	runID      string
	incidentID string
	actionID   string
	approvalID string
	agentToken string
}

// driveTier3SkipTaskProposal runs the whole producer side: apply a failing job,
// run it, wait for the incident the failure opens, mint a real agent-session
// token the way the session supervisor does, and propose `skip_task` through the
// agent tool surface. It asserts the proposal parked the incident in
// awaiting_approval with a pending approval — i.e. that the C4 wiring exists —
// before handing the ids back for a decision.
func (s *IntegrationTestSuite) driveTier3SkipTaskProposal(prefix string) tier3Proposal {
	s.T().Helper()

	alias := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	s.applyDefinition(failingJobDefinition(alias, s.engineType))

	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Require().Equal("failed", run.Status, "the gate step must fail so an incident opens")

	// A run failure emits both task_failed and run_failed; the run-level event
	// folds into the task's incident, but poll for the task-scoped one by name so
	// the scenario cannot latch onto a run-level twin if that folding regresses.
	incident := s.awaitIncidentForJobTask(job.ID, "gate", 60*time.Second)

	token := s.mintAgentSessionToken(incident.ID, alias)

	status, body := s.postWithToken(
		fmt.Sprintf("%s/v1/agent/incidents/%s/actions", s.caesiumURL, incident.ID),
		token,
		map[string]any{
			"type": "skip_task",
			"params": map[string]any{
				"task_name": "gate",
				"reason":    "approved remediation: gate step is non-essential today",
			},
		})
	s.Require().Equal(http.StatusAccepted, status, string(body))

	var proposal struct {
		Action      approvalAction `json:"action"`
		Disposition string         `json:"disposition"`
	}
	s.Require().NoError(json.Unmarshal(body, &proposal))
	s.Require().Equal("awaiting_approval", proposal.Disposition,
		"a tier-3 proposal must route to the approval gate, not execute and not merely record")
	s.Require().Equal(3, proposal.Action.Tier)
	s.Require().Equal("proposed", proposal.Action.Status)

	// The approval row and the parked incident are what make the proposal
	// actionable by a human. Their absence is exactly the bug this scenario
	// exists to prevent recurring.
	detail := s.awaitIncidentStatus(incident.ID, "awaiting_approval", 30*time.Second)
	s.Require().NotEmpty(detail.Approvals, "a tier-3 proposal must create an ApprovalRequest")

	var pending approvalRequest
	for _, a := range detail.Approvals {
		if a.ActionID == proposal.Action.ID && a.Decision == "pending" {
			pending = a
			break
		}
	}
	s.Require().NotEmpty(pending.ID, "no pending approval found for action %s", proposal.Action.ID)

	return tier3Proposal{
		jobID:      job.ID,
		runID:      runID,
		incidentID: incident.ID,
		actionID:   proposal.Action.ID,
		approvalID: pending.ID,
		agentToken: token,
	}
}

// mintAgentSessionToken mints a scoped agent-session credential directly against
// the live server's catalog, the same way incident.Supervisor does when it
// launches a session container (see test/agent_mcp_test.go). Driving the real
// token type is what makes the "an agent may not approve" assertion meaningful.
func (s *IntegrationTestSuite) mintAgentSessionToken(incidentID, alias string) string {
	s.T().Helper()

	vars := env.Variables()
	conn := s.openIntegrationCatalogGorm()
	authSvc := iauth.NewService(conn, iauth.WithKeyHashSecret(vars.AuthKeyHashSecret))

	id, err := uuid.Parse(incidentID)
	s.Require().NoError(err)
	key, err := authSvc.MintAgentSessionKey(id, []string{alias}, time.Hour)
	s.Require().NoError(err)
	s.Require().NotNil(key.Key)
	return key.Plaintext
}

// awaitIncidentForJobTask polls the operator feed until the incident for a
// specific (job, task) appears. The incident subscriber is leader-gated and
// consumes the failure asynchronously off the event bus, so this is a poll, not
// a read.
func (s *IntegrationTestSuite) awaitIncidentForJobTask(jobID, taskName string, timeout time.Duration) approvalIncident {
	s.T().Helper()

	deadline := time.Now().Add(timeout)
	var seen []string
	for {
		var list approvalIncidentList
		if err := s.tryGetJSON("/v1/incidents?job_id="+jobID, &list); err == nil {
			seen = seen[:0]
			for _, inc := range list.Incidents {
				if inc.JobID != jobID {
					continue
				}
				seen = append(seen, fmt.Sprintf("%s/%s", inc.TaskName, inc.Class))
				if inc.TaskName == taskName {
					return inc
				}
			}
		}
		if time.Now().After(deadline) {
			s.T().Fatalf("timeout waiting for an incident on job %s task %q (saw: %v)", jobID, taskName, seen)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// awaitIncidentStatus polls the incident timeline until the incident reaches the
// wanted status, returning the full detail.
func (s *IntegrationTestSuite) awaitIncidentStatus(incidentID, want string, timeout time.Duration) approvalDetail {
	s.T().Helper()

	deadline := time.Now().Add(timeout)
	var last approvalDetail
	for {
		var detail approvalDetail
		if err := s.tryGetJSON("/v1/incidents/"+incidentID, &detail); err == nil {
			last = detail
			if detail.Incident.Status == want {
				return detail
			}
		}
		if time.Now().After(deadline) {
			s.T().Fatalf("timeout waiting for incident %s to reach %q (last status %q)",
				incidentID, want, last.Incident.Status)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// incidentDetail reads the full incident timeline.
func (s *IntegrationTestSuite) incidentDetail(incidentID string) approvalDetail {
	s.T().Helper()
	var detail approvalDetail
	s.getJSON("/v1/incidents/"+incidentID, &detail)
	return detail
}

// awaitTaskStatus polls the run until one of its task rows reaches want.
// The approved action executes synchronously inside the approve request, but the
// run row is read back over HTTP, so a short poll keeps this free of a
// read-your-writes assumption about the catalog.
func (s *IntegrationTestSuite) awaitTaskStatus(jobID, runID, want string, timeout time.Duration) runResponse {
	s.T().Helper()

	deadline := time.Now().Add(timeout)
	var last runResponse
	for {
		var run runResponse
		if err := s.tryGetJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s", jobID, runID), &run); err == nil {
			last = run
			for _, task := range run.Tasks {
				if task.Status == want {
					return run
				}
			}
		}
		if time.Now().After(deadline) {
			statuses := make([]string, 0, len(last.Tasks))
			for _, task := range last.Tasks {
				statuses = append(statuses, task.Status)
			}
			s.T().Fatalf("timeout waiting for a task of run %s to reach %q (task statuses: %v)",
				runID, want, statuses)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// incidentCLIArgs builds `caesium incident <verb> <id> …` with the server and an
// operator key, and always asks for machine-readable output.
func (s *IntegrationTestSuite) incidentCLIArgs(verb, incidentID string, extra ...string) []string {
	args := []string{"incident", verb, incidentID, "--json", "--server", s.caesiumURL}
	if key := firstEnv("CAESIUM_INCIDENT_API_KEY", "CAESIUM_API_KEY", "CAESIUM_E2E_AUTH_ADMIN_KEY"); key != "" {
		args = append(args, "--api-key", key)
	}
	return append(args, extra...)
}

// postWithToken POSTs an optional JSON body with an explicit bearer token,
// bypassing the suite's default operator credential — the only way to prove that
// a specific principal (here, an agent-session token) is refused.
func (s *IntegrationTestSuite) postWithToken(url, token string, payload any) (int, []byte) {
	s.T().Helper()

	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		s.Require().NoError(err)
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(s.T().Context(), http.MethodPost, url, body)
	s.Require().NoError(err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer func() { _ = resp.Body.Close() }()

	out, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	return resp.StatusCode, out
}

// requireActionByID finds one action on the incident timeline.
func requireActionByID(s *IntegrationTestSuite, detail approvalDetail, actionID string) approvalAction {
	s.T().Helper()
	for _, action := range detail.Actions {
		if action.ID == actionID {
			return action
		}
	}
	types := make([]string, 0, len(detail.Actions))
	for _, action := range detail.Actions {
		types = append(types, fmt.Sprintf("%s=%s", action.Type, action.Status))
	}
	s.T().Fatalf("action %s not found on incident timeline (actions: %s)", actionID, strings.Join(types, ", "))
	return approvalAction{}
}

// TestIncidentEscalationDeliversNotifiableEvent drives the tier-1 `escalate`
// action through its real surface and asserts the escalation is DELIVERED, not
// merely recorded.
//
// This is the shape that hid the bug: the action row said `executed` with
// `"escalated": true` in its result while the server-side operation only wrote a
// log line, so an assertion on the action's status passed while nobody was ever
// contacted. The same call is what a git-synced job's approved jobdef patch
// degrades to, carrying the rendered diff — so "recorded but undelivered" meant
// an approved change nobody was told about. The assertion is therefore on the
// persisted incident_escalated event, which is what the notification subscriber
// routes to a channel.
func (s *IntegrationTestSuite) TestIncidentEscalationDeliversNotifiableEvent() {
	s.requireAuthLane()

	alias := fmt.Sprintf("escalate-delivery-%d", time.Now().UnixNano())
	s.applyDefinition(failingJobDefinition(alias, s.engineType))

	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Require().Equal("failed", run.Status, "the gate step must fail so an incident opens")

	incident := s.awaitIncidentForJobTask(job.ID, "gate", 60*time.Second)
	token := s.mintAgentSessionToken(incident.ID, alias)

	summary := fmt.Sprintf("vendor feed unrecoverable for %s; a human owns this", alias)
	status, body := s.postWithToken(
		fmt.Sprintf("%s/v1/agent/incidents/%s/actions", s.caesiumURL, incident.ID),
		token,
		map[string]any{
			"type": "escalate",
			"params": map[string]any{
				"channel": "oncall",
				"summary": summary,
			},
		})
	// 200, not the 202 a tier-3 proposal gets: escalate is tier 1, so it has
	// already run by the time the endpoint answers.
	s.Require().Equal(http.StatusOK, status, string(body))

	var proposal struct {
		Action      approvalAction `json:"action"`
		Disposition string         `json:"disposition"`
	}
	s.Require().NoError(json.Unmarshal(body, &proposal))
	s.Require().Equal("executed", proposal.Action.Status,
		"escalate is tier 1 and runs autonomously under the default playbook")

	// The delivery assertion. An escalate recorded `executed` whose event never
	// reached the stream is precisely the failure mode this scenario exists for.
	s.requireEscalationEvent(incident.ID, summary, 30*time.Second)
}

// requireEscalationEvent polls the persisted event stream for the
// incident_escalated event belonging to one incident. It reads /v1/events rather
// than the action row on purpose: the action row is what looked healthy while
// nothing was delivered.
func (s *IntegrationTestSuite) requireEscalationEvent(incidentID, wantSummary string, timeout time.Duration) {
	s.T().Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, evt := range s.readSSEBacklog("/v1/events", time.Second) {
			if evt.Type != event.TypeIncidentEscalated {
				continue
			}
			var payload struct {
				IncidentID string `json:"incident_id"`
				Channel    string `json:"channel"`
				Summary    string `json:"summary"`
				JobAlias   string `json:"job_alias"`
			}
			if err := json.Unmarshal(evt.Payload, &payload); err != nil {
				continue
			}
			if payload.IncidentID != incidentID {
				continue
			}
			s.Require().Equal("oncall", payload.Channel,
				"the escalation must name the channel the agent asked for")
			s.Require().Equal(wantSummary, payload.Summary,
				"the escalation body must reach the recipient, not just the action row")
			s.Require().NotEmpty(payload.JobAlias,
				"job_alias must be present so notification policies can filter on it")
			return
		}
	}
	s.Require().Fail("no incident_escalated event was delivered",
		"incident %s escalated but nothing reached the event stream", incidentID)
}

// TestIncidentApprovedActionRedriveRecoversAfterCrash gives the approval redrive sweeper
// its only integration coverage.
//
// The decision commits in its own transaction and the dispatch runs after it, so
// a process death in between strands the action `approved` forever: nothing
// retried it, and re-approving is refused by the pending-only guard. The
// ApprovalRedriver is the recovery path, and a leader-gated background sweep is
// exactly the kind of thing that passes unit tests while being unwired in
// production — so this drives the REAL sweeper on the live server.
//
// The crash is simulated the only way a black-box test can: after a real
// approval has executed, the action row is put back to `approved` (with an
// updated_at old enough to clear the redrive grace) and its effect undone,
// directly in the catalog. That is precisely the on-disk state a crash between
// commit and dispatch leaves behind.
func (s *IntegrationTestSuite) TestIncidentApprovedActionRedriveRecoversAfterCrash() {
	s.requireAuthLane()

	proposal := s.driveTier3SkipTaskProposal("approval-redrive")

	out, err := s.runCLIStdout(s.incidentCLIArgs("approve", proposal.incidentID,
		"--approval", proposal.approvalID, "--reason", "redrive coverage")...)
	s.Require().NoError(err, "caesium incident approve failed:\n%s", out)

	detail := s.incidentDetail(proposal.incidentID)
	s.Require().Equal("executed", requireActionByID(s, detail, proposal.actionID).Status,
		"the synchronous post-decision execute must run first; the redrive is the recovery path")

	conn := s.openIntegrationCatalogGorm()

	// Strand the action exactly as a crash between the decision commit and the
	// dispatch would: `approved`, no execution recorded, aged past the grace.
	stranded := time.Now().UTC().Add(-time.Hour)
	s.Require().NoError(conn.Exec(
		`UPDATE agent_actions SET status = ?, result = NULL, updated_at = ? WHERE id = ?`,
		"approved", stranded, proposal.actionID).Error)

	// Undo the effect too, so "the sweeper ran it" is observable rather than
	// inferred from a row that was already skipped.
	s.Require().NoError(conn.Exec(
		`UPDATE task_runs SET status = ?, error = ?, completed_at = NULL WHERE job_run_id = ?`,
		"failed", "caesium approval lane gate step", proposal.runID).Error)

	// The leader-gated sweeper must find it and dispatch it. The lane runs the
	// sweeper on a 5s interval (CAESIUM_AGENT_APPROVAL_REDRIVE_INTERVAL).
	s.Require().Eventually(func() bool {
		redriven := s.incidentDetail(proposal.incidentID)
		for _, a := range redriven.Actions {
			if a.ID == proposal.actionID {
				return a.Status == "executed"
			}
		}
		return false
	}, 90*time.Second, 2*time.Second,
		"the approval redrive sweeper must execute an approved action that was never dispatched")

	// The remediation actually happened again — the row moving is not the point,
	// the effect is.
	s.awaitTaskStatus(proposal.jobID, proposal.runID, "skipped", 60*time.Second)

	// The redriven execution still credits the recorded HUMAN decision: the
	// decider is read from the ApprovalRequest, never from whoever dispatched.
	final := requireActionByID(s, s.incidentDetail(proposal.incidentID), proposal.actionID)
	var result map[string]any
	s.Require().NoError(json.Unmarshal(final.Result, &result))
	s.Require().NotEmpty(result["approved_by"],
		"a redriven action must still name the human who approved it")
}

// TestIncidentApplyJobdefPatchCannotEditItsOwnPolicy is the self-modification refusal.
//
// `metadata.remediation` is persisted and IS the input to the effective-playbook
// resolver, so a patch that edits it is the agent rewriting the policy that
// governs the agent. The attack is quiet: propose a byte-identical definition
// plus a permissive remediation block, and a human approving what looks like a
// no-op hands the agent a wider allowlist for every later proposal. The refusal
// must therefore hold at the point of EXECUTION, after a human has approved —
// which is what this drives.
func (s *IntegrationTestSuite) TestIncidentApplyJobdefPatchCannotEditItsOwnPolicy() {
	s.requireAuthLane()

	alias := fmt.Sprintf("policy-selfedit-%d", time.Now().UnixNano())
	def := failingJobDefinition(alias, s.engineType)
	def.Metadata.Remediation = &schema.MetadataRemediation{
		Profile: "triage-only",
		Classes: []string{"unknown"},
	}
	s.applyDefinition(def)

	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	s.awaitRun(job.ID, runID, runTimeout)
	incident := s.awaitIncidentForJobTask(job.ID, "gate", 60*time.Second)
	token := s.mintAgentSessionToken(incident.ID, alias)

	// The patch: same job, but the agent grants itself every tier-2 action.
	escalated := def
	escalated.Metadata.Remediation = &schema.MetadataRemediation{
		Profile: "triage-only",
		Classes: []string{"unknown"},
		Autonomy: &schema.RemediationAutonomy{
			Allow: []string{"pause_job", "rerun_with_params", "clear_cache_entry"},
		},
	}
	patch, err := json.Marshal(escalated)
	s.Require().NoError(err)

	status, body := s.postWithToken(
		fmt.Sprintf("%s/v1/agent/incidents/%s/actions", s.caesiumURL, incident.ID),
		token,
		map[string]any{
			"type":   "apply_jobdef_patch",
			"params": map[string]any{"definition": json.RawMessage(patch)},
		})
	s.Require().Equal(http.StatusAccepted, status, string(body))

	var proposal struct {
		Action approvalAction `json:"action"`
	}
	s.Require().NoError(json.Unmarshal(body, &proposal))

	detail := s.awaitIncidentStatus(incident.ID, "awaiting_approval", 30*time.Second)
	var pending approvalRequest
	for _, a := range detail.Approvals {
		if a.ActionID == proposal.Action.ID && a.Decision == "pending" {
			pending = a
			break
		}
	}
	s.Require().NotEmpty(pending.ID, "no pending approval for the policy-editing patch")

	// A human approves it — the refusal must not depend on the human noticing.
	out, err := s.runCLIStdout(s.incidentCLIArgs("approve", incident.ID,
		"--approval", pending.ID, "--reason", "looks like a no-op")...)
	s.Require().NoError(err, "caesium incident approve failed:\n%s", out)

	executed := requireActionByID(s, s.incidentDetail(incident.ID), proposal.Action.ID)
	s.Require().Equal("failed", executed.Status,
		"an approved patch that edits the job's own remediation policy must be refused, not applied")
	var result map[string]any
	s.Require().NoError(json.Unmarshal(executed.Result, &result))
	s.Require().Contains(fmt.Sprint(result["error"]), "metadata.remediation",
		"the refusal must name what it refused: %v", result)

	// The job's policy is unchanged on the server.
	var jobs []struct {
		Alias       string          `json:"alias"`
		Remediation json.RawMessage `json:"remediation"`
	}
	s.getJSON("/v1/jobs", &jobs)
	for _, j := range jobs {
		if j.Alias != alias {
			continue
		}
		s.Require().NotContains(string(j.Remediation), "pause_job",
			"the agent must not have widened its own allowlist")
	}
}
