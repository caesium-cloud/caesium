//go:build integration

package test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/caesium-cloud/caesium/internal/connector"
	"github.com/caesium-cloud/caesium/internal/connector/correlation"
	ctemporal "github.com/caesium-cloud/caesium/internal/connector/temporal"
	"go.temporal.io/api/workflowservice/v1"
)

// TestConnectorsCorrelation qualifies the source/admission halves through real
// Temporal and public Caesium starts. The integration runner has no catalog DB
// handle: durable typed-link composition is covered separately by component
// tests; the public bidirectional link surface is C1's acceptance responsibility.
func (s *IntegrationTestSuite) TestConnectorsCorrelation() {
	if os.Getenv("CAESIUM_CONNECTORS_FIXTURE") != "true" {
		s.T().Skip("requires selected real connector fixture")
	}
	artifacts := os.Getenv("CAESIUM_CONNECTORS_ARTIFACTS")
	s.Require().NotEmpty(artifacts)
	candidate := os.Getenv("CAESIUM_CONNECTORS_CANDIDATE_SHA")
	s.Require().Len(candidate, 40)
	s.Require().NotEmpty(s.authAPIKey)
	var ready map[string]any
	s.fixtureRequest(http.MethodGet, "/health", nil, &ready)
	s.Require().Equal(true, ready["ready"])
	s.Require().Equal("1.49.0", ready["sdk"])
	s.Require().Equal("1.9.1", ready["cli"])
	s.Require().Equal("1.32.0", ready["server"])
	provenance := false
	for _, event := range s.fixtureEvidence() {
		if event["kind"] == "fixture_started" {
			s.Require().Equal(candidate, event["candidate"])
			s.Require().Equal(ready["sdk"], event["sdk"])
			s.Require().Equal(ready["cli"], event["cli"])
			s.Require().Equal(ready["server"], event["server"])
			provenance = true
		}
	}
	s.Require().True(provenance, "fixture startup journal must name this candidate and version matrix")
	ctx, cancel := context.WithTimeout(s.T().Context(), 5*time.Second)
	defer cancel()
	c, err := ctemporal.Dial(ctx, ctemporal.DialConfig{Endpoint: "127.0.0.1:7234", Namespace: "default", CertificatePaths: []string{filepath.Join(artifacts, "tls/ca.crt"), filepath.Join(artifacts, "tls/tls.crt"), filepath.Join(artifacts, "tls/tls.key")}})
	s.Require().NoError(err)
	defer c.Close()
	system, err := c.WorkflowService().GetSystemInfo(s.T().Context(), &workflowservice.GetSystemInfoRequest{})
	s.Require().NoError(err)
	s.Require().Equal(ready["server"], system.GetServerVersion())
	observer, err := ctemporal.NewObserverFromClient(c, "default", "", 3*time.Second, 100, 65536)
	s.Require().NoError(err)
	alias := fmt.Sprintf("connector-correlation-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf("apiVersion: v1\nkind: Job\nmetadata:\n  alias: %s\n  concurrency:\n    maxRuns: 1\n    strategy: queue\ntrigger:\n  type: cron\n  configuration:\n    cron: '0 0 1 1 *'\nsteps:\n  - name: work\n    image: alpine:3.23\n    command: [sh, -c, 'sleep 8; echo correlation-work']\n", alias)
	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runner := s.createAPIKeyCLI("--role", "runner", "--scope-jobs", alias, "--description", "correlation fixture runner")
	s.Require().Equal(http.StatusOK, s.fixtureRequest(http.MethodPost, "/configure", map[string]string{"runner_key": runner.Plaintext}, nil))
	binding := connector.Binding{Name: "publication", Version: "1", ActivityAllowlist: []connector.ActivityJob{{ActivityType: "caesium.start", Jobs: []string{alias}}}}
	holder := s.postRunStart(job.ID, "correlation-holder", `{"params":{}}`)
	s.Require().Equal(http.StatusAccepted, holder.status)
	s.Require().Equal("created", holder.str("outcome"))
	s.Require().NotEmpty(holder.str("id"))
	ref := s.fixtureStart("correlation-queue", "publication", job.ID, job.ID, true, true, false, false)
	var queued connector.CorrelationEnvelope
	s.Require().Eventually(func() bool {
		for _, event := range s.fixtureEvidence() {
			if event["kind"] == "http_start" && event["workflow_id"] == ref.WorkflowID {
				raw, _ := json.Marshal(event["hint"])
				var hint connector.CorrelationEnvelope
				if json.Unmarshal(raw, &hint) == nil && hint.Outcome == "queued" {
					queued = hint
					return true
				}
			}
		}
		return false
	}, 20*time.Second, 100*time.Millisecond, "real fixture must report a queued admission behind an actual holder")
	s.Require().Equal(job.ID, queued.JobID)
	s.Require().NotEmpty(queued.QueueID)
	s.Require().Empty(queued.RunID)
	// The key comes from actual provider identity, even while a queued activity
	// has not finished and no default JSON result exists in history yet.
	pending, err := observer.CorrelationActivities(s.T().Context(), "fixture", ref.WorkflowID, ref.RunID, binding, nil, 100)
	s.Require().NoError(err)
	s.Require().Len(pending.Activities, 1)
	source := pending.Activities[0]
	s.Require().Equal(ref.RunID+"/"+source.ActivityID, queued.IdempotencyKey)
	s.Require().Equal(queued.IdempotencyKey, source.IdempotencyKey)
	s.Require().Positive(source.SourceEventID)
	s.Require().NotEmpty(source.SourceEventType)
	state := s.fixtureAwait(c, ref, "awaiting_approval")
	s.Require().NotNil(state.First)
	s.Require().Equal("created", state.First.Outcome)
	s.Require().Equal(queued.IdempotencyKey, state.First.IdempotencyKey)
	s.Require().NotEmpty(state.First.RunID)
	replay := s.postRunStart(job.ID, queued.IdempotencyKey, `{"params":{}}`)
	s.Require().Equal(http.StatusAccepted, replay.status)
	s.Require().Equal("true", replay.replayed)
	s.Require().Equal(state.First.RunID, replay.str("id"))
	s.Require().Equal("created", replay.str("outcome"))
	s.Require().Len(s.fetchRuns(job.ID), 2, "holder plus exactly one promoted run despite activity retries")
	finished, err := observer.CorrelationActivities(s.T().Context(), "fixture", ref.WorkflowID, ref.RunID, binding, nil, 100)
	s.Require().NoError(err)
	s.Require().Len(finished.Activities, 1)
	s.Require().Equal(state.First, finished.Activities[0].Hint)
	s.Require().Equal(source.ActivityID, finished.Activities[0].ActivityID)
	starts := 0
	retryActivated := false
	queueSeen := false
	createdSeen := false
	for _, event := range s.fixtureEvidence() {
		if event["kind"] == "http_start" && event["workflow_id"] == ref.WorkflowID {
			starts++
			hint := event["hint"].(map[string]any)
			s.Require().Equal(queued.IdempotencyKey, hint["idempotency_key"])
			if event["attempt"] == float64(2) {
				retryActivated = true
			}
			queueSeen = queueSeen || hint["outcome"] == "queued"
			createdSeen = createdSeen || hint["outcome"] == "created"
		}
	}
	s.Require().Greater(starts, 1)
	s.Require().True(retryActivated)
	s.Require().True(queueSeen)
	s.Require().True(createdSeen)
	s.fixtureRequest(http.MethodPost, "/worker", map[string]bool{"running": false}, nil)
	s.fixtureRequest(http.MethodPost, "/worker", map[string]bool{"running": true}, nil)
	resumed := s.fixtureAwait(c, ref, "awaiting_approval")
	s.Require().Equal(state.First, resumed.First)
	// Opaque custom conversion must still expose trusted source identity without
	// decoding or retaining the payload. The independent fixture journal proves
	// the actual public admission under that provider-derived key.
	opaque := s.fixtureStart("correlation-opaque", "publication", job.ID, job.ID, true, false, true, false)
	opaqueState := s.fixtureAwait(c, opaque, "awaiting_approval")
	s.Require().Nil(opaqueState.First)
	opaquePage, err := observer.CorrelationActivities(s.T().Context(), "fixture", opaque.WorkflowID, opaque.RunID, binding, nil, 100)
	s.Require().NoError(err)
	s.Require().Len(opaquePage.Activities, 1)
	s.Require().Nil(opaquePage.Activities[0].Hint)
	opaqueKey := opaquePage.Activities[0].IdempotencyKey
	s.Require().Equal(opaque.RunID+"/"+opaquePage.Activities[0].ActivityID, opaqueKey)
	opaqueReplay := s.postRunStart(job.ID, opaqueKey, `{"params":{}}`)
	s.Require().Equal(http.StatusAccepted, opaqueReplay.status)
	s.Require().Equal("true", opaqueReplay.replayed)
	s.Require().NotEmpty(opaqueReplay.str("id"))
	s.Require().Len(s.fetchRuns(job.ID), 3)
	wrongBinding := binding
	wrongBinding.Name = "unrelated"
	_, err = observer.CorrelationActivities(s.T().Context(), "fixture", ref.WorkflowID, ref.RunID, wrongBinding, nil, 100)
	s.Require().ErrorIs(err, correlation.ErrUnverifiedCorrelation)
	unrelated := binding
	unrelated.ActivityAllowlist = []connector.ActivityJob{{ActivityType: "unrelated", Jobs: []string{alias}}}
	unrelatedPage, err := observer.CorrelationActivities(s.T().Context(), "fixture", ref.WorkflowID, ref.RunID, unrelated, nil, 100)
	s.Require().NoError(err)
	s.Require().Empty(unrelatedPage.Activities)
	raw, err := json.MarshalIndent(map[string]any{"candidate_sha": os.Getenv("CAESIUM_CONNECTORS_CANDIDATE_SHA"), "scope": "real provider source and actual public admissions; durable catalog composition covered by component tests; C1 public links pending", "queued": queued, "source": source, "finished": finished, "opaque": opaquePage, "promoted_run_id": state.First.RunID, "opaque_run_id": opaqueReplay.str("id"), "activity_retry_activated": retryActivated}, "", "  ")
	s.Require().NoError(err)
	s.Require().NoError(os.WriteFile(filepath.Join(artifacts, "correlation-evidence.json"), raw, 0644))
}
