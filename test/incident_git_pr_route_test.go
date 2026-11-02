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

	"github.com/caesium-cloud/caesium/internal/event"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
)

// TestIncidentGitProvenancePatchDegradesToEscalate drives data-circuit-breaker
// F3's degrade through the real surface. The lane configures no
// CAESIUM_GIT_WRITE_CREDENTIALS, so an approved patch of a job with git provenance
// cannot open a pull request. It must not half-succeed and must not fall back to
// a direct edit: the human is told, with the diff, and the job stays as it was.
// The approver sees that outcome before deciding.
//
// The live pull request path is unit-tested against a fake forge
// (internal/incident/provenance_test.go); an integration test cannot open a real
// one (plan Open Question 5).
func (s *IntegrationTestSuite) TestIncidentGitProvenancePatchDegradesToEscalate() {
	s.requireAuthLane()

	alias := fmt.Sprintf("git-pr-degrade-%d", time.Now().UnixNano())
	def := failingJobDefinition(alias, s.engineType)
	def.Metadata.Remediation = &schema.MetadataRemediation{Profile: "triage-only", Classes: []string{"unknown"}}

	payload, err := json.Marshal(map[string]any{
		"definitions": []schema.Definition{def},
		"provenance": map[string]string{
			"source_id": "orders-source",
			"repo":      "https://github.com/acme/jobs.git",
			"ref":       "main",
			"commit":    strings.Repeat("a", 40),
			"path":      "pipelines/orders.job.yaml",
		},
	})
	s.Require().NoError(err)
	resp, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/jobdefs/apply", bytes.NewReader(payload))
	s.Require().NoError(err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, string(body))

	job := s.requireJobByAlias(alias)
	s.awaitRun(job.ID, s.triggerRun(job.ID), runTimeout)
	incident := s.awaitIncidentForJobTask(job.ID, "gate", 60*time.Second)
	token := s.mintAgentSessionToken(incident.ID, alias)

	patched := def
	step := def.Steps[0]
	step.Command = []string{"sh", "-c", "echo git-pr-degrade-patched"}
	patched.Steps = []schema.Step{step}
	patch, err := json.Marshal(patched)
	s.Require().NoError(err)

	status, proposed := s.postWithToken(
		fmt.Sprintf("%s/v1/agent/incidents/%s/actions", s.caesiumURL, incident.ID),
		token,
		map[string]any{
			"type":   "apply_jobdef_patch",
			"params": map[string]any{"definition": json.RawMessage(patch)},
		})
	s.Require().Equal(http.StatusAccepted, status, string(proposed))
	var proposal struct {
		Action approvalAction `json:"action"`
	}
	s.Require().NoError(json.Unmarshal(proposed, &proposal))

	// The approver reads the outcome before deciding, from the same row.
	detail := s.awaitIncidentStatus(incident.ID, "awaiting_approval", 30*time.Second)
	var preview struct {
		Preview bool   `json:"preview"`
		Route   string `json:"route"`
		Degrade string `json:"degrade"`
		Diff    string `json:"diff"`
	}
	s.Require().NoError(json.Unmarshal(requireActionByID(s, detail, proposal.Action.ID).Result, &preview))
	s.Require().True(preview.Preview)
	s.Require().Equal("escalate", preview.Route)
	s.Require().Equal("no_write_credential", preview.Degrade)
	s.Require().Contains(preview.Diff, "git-pr-degrade-patched", "the approval surface shows the change")

	var pending string
	for _, a := range detail.Approvals {
		if a.ActionID == proposal.Action.ID && a.Decision == "pending" {
			pending = a.ID
		}
	}
	s.Require().NotEmpty(pending, "no pending approval for the patch")
	out, err := s.runCLIStdout(s.incidentCLIArgs("approve", incident.ID,
		"--approval", pending, "--reason", "patch the manifest")...)
	s.Require().NoError(err, "caesium incident approve failed:\n%s", out)

	// Read back over HTTP: executed, as an escalation, saying why.
	executed := requireActionByID(s, s.incidentDetail(incident.ID), proposal.Action.ID)
	s.Require().Equal("executed", executed.Status, string(executed.Result))
	var result struct {
		Preview bool            `json:"preview"`
		Route   string          `json:"route"`
		Applied bool            `json:"applied"`
		Degrade string          `json:"degrade"`
		Reason  string          `json:"reason"`
		Diff    json.RawMessage `json:"diff"`
		PRURL   string          `json:"pr_url"`
	}
	s.Require().NoError(json.Unmarshal(executed.Result, &result))
	s.False(result.Preview, "the executed result replaces the preview")
	s.Equal("escalate", result.Route)
	s.False(result.Applied)
	s.Equal("no_write_credential", result.Degrade)
	s.Contains(result.Reason, "CAESIUM_GIT_WRITE_CREDENTIALS")
	s.Contains(string(result.Diff), "git-pr-degrade-patched", "the degrade carries the rendered diff")
	s.Empty(result.PRURL)

	// The escalation a human receives names why and carries the same diff.
	s.Eventually(func() bool {
		for _, evt := range s.readSSEBacklog("/v1/events", time.Second) {
			var payload struct {
				IncidentID string `json:"incident_id"`
				Summary    string `json:"summary"`
			}
			if evt.Type != event.TypeIncidentEscalated || json.Unmarshal(evt.Payload, &payload) != nil {
				continue
			}
			if payload.IncidentID == incident.ID {
				return strings.Contains(payload.Summary, "CAESIUM_GIT_WRITE_CREDENTIALS") &&
					strings.Contains(payload.Summary, "git-pr-degrade-patched")
			}
		}
		return false
	}, 30*time.Second, time.Second, "no incident_escalated event carried the reason and the diff")

	// No silent server-side edit: the stored job still runs the old command.
	manifest := s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs/"+job.ID+"/manifest", s.authAPIKey, "")
	s.Contains(manifest, "caesium approval lane gate step")
	s.NotContains(manifest, "git-pr-degrade-patched")
}
