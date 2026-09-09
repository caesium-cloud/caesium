//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
)

// incident_perclass_test.go drives `metadata.remediation.autonomy.perClass` —
// per-failure-class autonomy narrowing — through its real surface on the
// auth-enabled remediation lane (issue #416).
//
// The gap it covers is the kind that passes every unit test: the schema
// accepted `perClass`, `caesium job lint` validated the class names, the block
// was persisted on the job… and the executor's playbook decoder never read the
// key. A job that declared "always ask a human before escalating an `unknown`
// failure" was enforced as if it had named no classes at all — silently coarser
// than declared, in the direction that grants more.
//
// So both scenarios assert on the OBSERVED disposition of a real agent proposal
// against a live server, and on the escalation actually being withheld and then
// delivered — never on a row that merely says the right word.

// perClassProposal is one `escalate` proposal driven under a job-declared
// remediation policy, plus everything needed to assert what the server did with
// it.
type perClassProposal struct {
	jobID      string
	incidentID string
	status     int
	body       []byte
	summary    string
}

// proposeEscalateUnderPolicy applies a failing job carrying `block`, runs it,
// waits for the incident its failure opens, and proposes a tier-1 `escalate`
// through the real agent tool surface with a real agent-session token.
//
// `escalate` is the right probe for per-class narrowing: it is tier 1 and the
// shipped `triage-only` profile allows it, so under the job's base policy it
// executes autonomously. Anything other than "executed" therefore had to come
// from the per-class block, which is exactly what is under test.
func (s *IntegrationTestSuite) proposeEscalateUnderPolicy(prefix string, block *schema.MetadataRemediation) perClassProposal {
	s.T().Helper()

	alias := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	def := failingJobDefinition(alias, s.engineType)
	def.Metadata.Remediation = block
	s.applyDefinition(def)

	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Require().Equal("failed", run.Status, "the gate step must fail so an incident opens")

	incident := s.awaitIncidentForJobTask(job.ID, "gate", 60*time.Second)

	// The whole scenario turns on WHICH class the failure produced: one policy
	// names it, the other deliberately does not. Assert it rather than assume
	// it, so a classifier change fails loudly instead of quietly making both
	// halves of this test vacuous.
	s.Require().Equal("unknown", incident.Class,
		"a bare `exit 1` must classify as `unknown` for the per-class keys below to mean what they say")

	token := s.mintAgentSessionToken(incident.ID, alias)
	summary := fmt.Sprintf("per-class policy probe for %s", alias)
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

	return perClassProposal{
		jobID:      job.ID,
		incidentID: incident.ID,
		status:     status,
		body:       body,
		summary:    summary,
	}
}

// TestIncidentPerClassNarrowingGatesTheMatchingClass: a job that requires
// approval for the failure class it actually hit must have that action parked
// for a human, not auto-executed — and the escalation must genuinely not have
// been delivered while it waits.
func (s *IntegrationTestSuite) TestIncidentPerClassNarrowingGatesTheMatchingClass() {
	s.requireAuthLane()

	gated := s.proposeEscalateUnderPolicy("perclass-gated", &schema.MetadataRemediation{
		Profile: "triage-only",
		Classes: []string{"unknown"},
		Autonomy: &schema.RemediationAutonomy{
			// No `allow` of its own: the job inherits triage-only's
			// `allow: [escalate]` and narrows it for THIS class only. If the
			// block were ignored, escalate would run autonomously — which is
			// precisely what it did before #416 was fixed.
			PerClass: map[string]schema.RemediationClassPolicy{
				"unknown": {RequireApproval: []string{"escalate"}},
			},
		},
	})

	s.Require().Equal(http.StatusAccepted, gated.status, string(gated.body))

	var proposal struct {
		Action      approvalAction `json:"action"`
		Disposition string         `json:"disposition"`
	}
	s.Require().NoError(json.Unmarshal(gated.body, &proposal))
	s.Require().Equal(1, proposal.Action.Tier,
		"escalate is tier 1, so only the per-class gate can hold it back")
	s.Require().Equal("proposed", proposal.Action.Status,
		"a per-class approval gate must park the action, not execute it")
	s.Require().Equal("awaiting_approval", proposal.Disposition,
		"the endpoint must tell the agent a human now owns this, not merely `proposed`")

	detail := s.awaitIncidentStatus(gated.incidentID, "awaiting_approval", 30*time.Second)
	s.Require().NotEmpty(detail.Approvals,
		"a per-class approval gate must create an ApprovalRequest a human can decide")

	var pending approvalRequest
	for _, a := range detail.Approvals {
		if a.ActionID == proposal.Action.ID && a.Decision == "pending" {
			pending = a
			break
		}
	}
	s.Require().NotEmpty(pending.ID, "no pending approval found for action %s", proposal.Action.ID)

	// The row saying `proposed` is not proof nobody was paged. Assert the
	// escalation was actually WITHHELD — the action row looking healthy while
	// the side effect happened anyway is the failure mode this lane exists for.
	s.requireNoEscalationEvent(gated.incidentID, 2*time.Second)

	// …and that the gate DEFERRED the escalation rather than dropping it: once a
	// human approves, the same action runs and the escalation is delivered.
	out, err := s.runCLIStdout(s.incidentCLIArgs("approve", gated.incidentID,
		"--approval", pending.ID, "--reason", "per-class gate reviewed")...)
	s.Require().NoError(err, "caesium incident approve failed:\n%s", out)

	var decided approvalRequest
	s.Require().NoError(json.Unmarshal([]byte(out), &decided))
	s.Require().Equal("approved", decided.Decision)

	executed := requireActionByID(s, s.incidentDetail(gated.incidentID), proposal.Action.ID)
	s.Require().Equal("executed", executed.Status,
		"an approved, class-gated action must run — the gate defers, it does not discard")
	s.requireEscalationEvent(gated.incidentID, gated.summary, 30*time.Second)
}

// TestIncidentPerClassNarrowingIgnoresNonMatchingClass is the inverse, and it is
// what stops the fix from being "gate everything": a per-class block keyed on a
// class the incident is NOT must leave the base policy alone.
func (s *IntegrationTestSuite) TestIncidentPerClassNarrowingIgnoresNonMatchingClass() {
	s.requireAuthLane()

	open := s.proposeEscalateUnderPolicy("perclass-other", &schema.MetadataRemediation{
		Profile: "triage-only",
		Classes: []string{"unknown", "auth_failure"},
		Autonomy: &schema.RemediationAutonomy{
			// The gate is declared for auth_failure; this incident is `unknown`,
			// so nothing narrows and triage-only's `allow: [escalate]` governs.
			PerClass: map[string]schema.RemediationClassPolicy{
				"auth_failure": {RequireApproval: []string{"escalate"}},
			},
		},
	})

	// 200, not the 202 a parked proposal gets: it already ran.
	s.Require().Equal(http.StatusOK, open.status, string(open.body))

	var proposal struct {
		Action      approvalAction `json:"action"`
		Disposition string         `json:"disposition"`
	}
	s.Require().NoError(json.Unmarshal(open.body, &proposal))
	s.Require().Equal("executed", proposal.Action.Status,
		"a per-class gate for a class this incident is not must not constrain it")
	s.Require().Equal("executed", proposal.Disposition)

	detail := s.incidentDetail(open.incidentID)
	s.Require().Empty(detail.Approvals,
		"no approval may be created for a class the policy did not name")

	// And the escalation really was delivered, not merely recorded.
	s.requireEscalationEvent(open.incidentID, open.summary, 30*time.Second)
}

// requireNoEscalationEvent fails if an incident_escalated event for this
// incident reaches the stream within the window. It is the negative twin of
// requireEscalationEvent: "the action row says proposed" and "nobody was paged"
// are different claims, and only the second one is the safety property.
func (s *IntegrationTestSuite) requireNoEscalationEvent(incidentID string, window time.Duration) {
	s.T().Helper()

	for _, evt := range s.readSSEBacklog("/v1/events", window) {
		if evt.Type != event.TypeIncidentEscalated {
			continue
		}
		var payload struct {
			IncidentID string `json:"incident_id"`
		}
		if err := json.Unmarshal(evt.Payload, &payload); err != nil {
			continue
		}
		s.Require().NotEqual(incidentID, payload.IncidentID,
			"a class-gated escalate must not deliver while it waits for a human")
	}
}
