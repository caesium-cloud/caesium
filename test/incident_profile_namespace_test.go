//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/incident"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
)

// incident_profile_namespace_test.go: runtime agent-profile resolution is
// namespace-aware (identity-and-access D4). An incident resolves only a
// profile in its own persisted namespace or "*"; any other is treated as a
// missing one. Lint refuses a cross-namespace reference at apply, so the
// foreign reference here is produced the way it arises in practice: the job
// moves to another namespace (and to that namespace's profile) after its
// incident opened.

// profileProbe is one failed run's incident under a job-declared profile,
// probed through the bundle and the agent action surface.
type profileProbe struct {
	incidentID string
	allow      []string
	status     string
	summary    string
}

func (s *IntegrationTestSuite) d4Profile(namespace, name string) {
	s.T().Helper()
	body := fmt.Sprintf(`{"namespace":%q,"name":%q,"image":"busybox:1.36.1","playbook":{"autonomy":{"allow":["escalate"]}}}`, namespace, name)
	id := decodeID(s, s.requireNSStatus(http.StatusCreated, http.MethodPost, "/v1/agentprofiles", s.authAPIKey, body))
	s.T().Cleanup(func() { s.nsRequest(http.MethodDelete, "/v1/agentprofiles/"+id, s.authAPIKey, "", nil) })
}

// openIncidentUnder applies a failing marketing job declaring profile, fails
// a run, waits for the marketing incident, then (when movedTo is set)
// re-applies the job in movedTo declaring movedProfile with movedLabels.
func (s *IntegrationTestSuite) openIncidentUnder(alias, profile, movedTo, movedProfile string, movedLabels map[string]string) (approvalIncident, *jobSummary) {
	s.T().Helper()
	def := failingJobDefinition(alias, s.engineType)
	def.Metadata.Namespace = "marketing"
	def.Metadata.Remediation = &schema.MetadataRemediation{Profile: profile, Classes: []string{"unknown"}}
	s.applyDefinition(def)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	s.Require().Equal("failed", s.awaitRun(job.ID, runID, runTimeout).Status)
	inc := s.awaitIncidentForJobTask(job.ID, "gate", 60*time.Second)

	if movedTo != "" {
		moved := failingJobDefinition(alias, s.engineType)
		moved.Metadata.Namespace = movedTo
		moved.Metadata.Labels = movedLabels
		moved.Metadata.Remediation = &schema.MetadataRemediation{Profile: movedProfile, Classes: []string{"unknown"}}
		s.applyDefinition(moved)
		s.Require().Equal(job.ID, s.requireJobByAlias(alias).ID, "the move keeps the job")
	}
	return inc, job
}

func (s *IntegrationTestSuite) probeProfile(inc approvalIncident, alias string) profileProbe {
	s.T().Helper()
	probe := profileProbe{incidentID: inc.ID, summary: fmt.Sprintf("d4 profile probe for %s", alias)}

	// The triage bundle shows the agent the policy that will be enforced.
	status, body := s.requestWithKey(http.MethodGet, "/v1/agent/incidents/"+inc.ID+"/bundle", s.authAPIKey, nil)
	s.Require().Equal(http.StatusOK, status, body)
	var bundle incident.Bundle
	s.Require().NoError(json.Unmarshal([]byte(body), &bundle))
	var playbook struct {
		Autonomy struct {
			Allow []string `json:"allow"`
		} `json:"autonomy"`
	}
	s.Require().NoError(json.Unmarshal(bundle.Playbook, &playbook), string(bundle.Playbook))
	probe.allow = playbook.Autonomy.Allow

	// The action surface enforces it: escalate is tier 1 and allowed by every
	// profile here, so only a refused profile stops it.
	token := s.mintAgentSessionToken(inc.ID, alias)
	code, raw := s.postWithToken(fmt.Sprintf("%s/v1/agent/incidents/%s/actions", s.caesiumURL, inc.ID), token,
		map[string]any{"type": "escalate", "params": map[string]any{"channel": "oncall", "summary": probe.summary}})
	s.Require().Equal(http.StatusOK, code, string(raw))
	var proposal struct {
		Action approvalAction `json:"action"`
	}
	s.Require().NoError(json.Unmarshal(raw, &proposal))
	probe.status = proposal.Action.Status
	return probe
}

// TestIncidentProfileOutsideNamespaceIsRefused: a marketing incident resolves
// its own namespace's profile and a shared one; once its job moved to finance
// and names finance's profile, the job's remediation block is ignored (A8)
// and the incident runs under the deployment default. The shared-profile escalation is delivered to the
// marketing policy, not the finance one, and without the job's new labels.
func (s *IntegrationTestSuite) TestIncidentProfileOutsideNamespaceIsRefused() {
	s.requireAuthLane()
	stamp := time.Now().UnixNano()
	mktProfile := fmt.Sprintf("d4-prof-mkt-%d", stamp)
	finProfile := fmt.Sprintf("d4-prof-fin-%d", stamp)
	s.d4Profile("marketing", mktProfile)
	s.d4Profile("finance", finProfile)

	// Same namespace: resolved.
	alias := fmt.Sprintf("d4-prof-own-%d", stamp)
	inc, _ := s.openIncidentUnder(alias, mktProfile, "", "", nil)
	own := s.probeProfile(inc, alias)
	s.Equal([]string{"escalate"}, own.allow, "the incident's own namespace's profile is its playbook")
	s.Equal("executed", own.status)

	// Moved job naming a finance profile: the remediation block the job's new
	// owner wrote is not consulted at all (A8), so the incident resolves as a
	// job declaring no remediation does, under the deployment default
	// (triage-only, shared).
	alias = fmt.Sprintf("d4-prof-moved-%d", stamp)
	inc, _ = s.openIncidentUnder(alias, mktProfile, "finance", finProfile, nil)
	moved := s.probeProfile(inc, alias)
	s.Equal([]string{"escalate"}, moved.allow, "a moved job's incident runs under the deployment default, not the new owner's profile")
	s.Equal("executed", moved.status)

	// Moved job naming the shared triage-only profile: resolved. Its
	// escalation belongs to the marketing incident.
	s.skipWithoutHostWebhooks()
	alias = fmt.Sprintf("d4-prof-shared-%d", stamp)
	marker := fmt.Sprintf("finance-only-label-%d", stamp)
	def := failingJobDefinition(alias, s.engineType)
	def.Metadata.Namespace = "marketing"
	def.Metadata.Remediation = &schema.MetadataRemediation{Profile: "triage-only", Classes: []string{"unknown"}}
	s.applyDefinition(def)
	job := s.requireJobByAlias(alias)
	cleanup := &sharedObjectCleanup{s: s}
	defer cleanup.run()
	captures := map[string]*d4Capture{}
	for _, namespace := range []string{"marketing", "finance"} {
		capture := newD4Capture()
		defer capture.Close()
		captures[namespace] = capture
		channel := s.d4WebhookChannel(s.authAPIKey, namespace, fmt.Sprintf("d4-esc-%s-%d", namespace, stamp), capture.URL())
		cleanup.add("/v1/notifications/channels/" + channel)
		policy := s.d4Policy(s.authAPIKey, namespace, fmt.Sprintf("d4-esc-%s-%d", namespace, stamp), channel, "incident_escalated", job.ID)
		cleanup.add("/v1/notifications/policies/" + policy)
	}
	inc, _ = s.openIncidentUnder(alias, "triage-only", "finance", "triage-only", map[string]string{"owner": marker})
	shared := s.probeProfile(inc, alias)
	s.Equal([]string{"escalate"}, shared.allow, "a shared profile resolves for every namespace")
	s.Equal("executed", shared.status)
	s.requireEscalationEvent(inc.ID, shared.summary, 30*time.Second)
	delivered := func(namespace string) []string {
		var hits []string
		for _, body := range captures[namespace].Snapshot() {
			if strings.Contains(body, inc.ID) {
				hits = append(hits, body)
			}
		}
		return hits
	}
	s.Require().Eventually(func() bool { return len(delivered("marketing")) > 0 }, 30*time.Second, 250*time.Millisecond,
		"the marketing incident's escalation reaches marketing's policy after the job moved to finance")
	s.Require().Never(func() bool { return len(delivered("finance")) > 0 }, 3*time.Second, 250*time.Millisecond,
		"the marketing incident's escalation never reaches the job's new namespace")
	s.False(slices.ContainsFunc(delivered("marketing"), func(body string) bool { return strings.Contains(body, marker) }),
		"the job's new owner's labels must not reach the old owner")
}
