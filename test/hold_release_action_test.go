//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/incident"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

// heldHoldRow is a row of GET /v1/datasets/holds.
type heldHoldRow struct {
	ID            string          `json:"id"`
	Status        string          `json:"status"`
	ReleasedBy    string          `json:"released_by"`
	ReleaseReason string          `json:"release_reason"`
	ReleaseNote   string          `json:"release_note"`
	Tolerances    json.RawMessage `json:"tolerances"`
}

func (s *IntegrationTestSuite) holdRow(dataset string) heldHoldRow {
	s.T().Helper()
	var holds struct {
		Holds []heldHoldRow `json:"holds"`
	}
	s.getJSON("/v1/datasets/holds?status=all&name="+url.QueryEscape(dataset), &holds)
	s.Require().Len(holds.Holds, 1)
	return holds.Holds[0]
}

// proposedAction is the body of POST /v1/agent/incidents/:id/actions.
type proposedAction struct {
	Action      approvalAction `json:"action"`
	Disposition string         `json:"disposition"`
}

func (s *IntegrationTestSuite) proposeRelease(incidentID, token string, params map[string]any) (int, proposedAction, string) {
	s.T().Helper()
	status, body := s.postWithToken(
		fmt.Sprintf("%s/v1/agent/incidents/%s/actions", s.caesiumURL, incidentID), token,
		map[string]any{"type": "release_hold", "params": params})
	var proposal proposedAction
	s.Require().NoError(json.Unmarshal(body, &proposal), string(body))
	return status, proposal, string(body)
}

func (s *IntegrationTestSuite) callMCPTool(incidentID, token, tool string, arguments map[string]any) []byte {
	s.T().Helper()
	id, err := uuid.Parse(incidentID)
	s.Require().NoError(err)
	status, body := s.postAgentMCP(token, id, map[string]any{
		"jsonrpc": "2.0",
		"id":      tool,
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": arguments},
	})
	s.Require().Equal(http.StatusOK, status, string(body))
	return body
}

// holdFixture is a real breach of a manual-release dataset and the incident it
// opened. A breach after clean samples, so the metric series has both.
type holdFixture struct {
	alias    string
	dataset  string
	producer *jobSummary
	incident holdIncident
	holdID   string
}

func (s *IntegrationTestSuite) breachProducer(namespace, prefix, remediation string) holdFixture {
	s.T().Helper()
	stamp := time.Now().UnixNano()
	f := holdFixture{alias: fmt.Sprintf("%s-%d", prefix, stamp), dataset: fmt.Sprintf("%s.orders.%d", prefix, stamp)}
	s.cleanupInOrder(f.alias)

	apply := func(rowCount float64) *jobSummary {
		step, err := metricsProducerStepForDataset("load", f.dataset, map[string]any{"rowCount": rowCount})
		s.Require().NoError(err)
		manifest := holdProducerManifest(f.alias, f.dataset, step, "manual")
		manifest = strings.Replace(manifest, "  cache: false", "  namespace: "+namespace+"\n  cache: false"+remediation, 1)
		return s.applyAssertionsJob(f.alias, manifest)
	}
	f.producer = apply(5000)
	s.runProducer(f.producer)
	f.producer = apply(12)
	s.runProducer(f.producer)

	f.incident = s.awaitHoldIncidents(f.producer.ID, 1)[0]
	var evidence holdIncidentEvidence
	s.Require().NoError(json.Unmarshal(f.incident.Evidence, &evidence))
	s.Require().NotEmpty(evidence.HoldID)
	f.holdID = evidence.HoldID
	return f
}

// TestHoldReleaseActionRunsThroughApproval is data-circuit-breaker F2 on the real
// surface. A real breach opens a real incident. The agent reads the hold from its
// bundle and its metric series from the context route and MCP, cannot reach another
// tenant's hold or dataset, and proposes the release with a tolerance through the
// MCP tool. A human approves it with the CLI, and the dataset is seen released over
// HTTP with the incident closed. It needs no policy file, so it also runs on the
// coverage auth lane.
func (s *IntegrationTestSuite) TestHoldReleaseActionRunsThroughApproval() {
	s.requireAuthLane()
	s.requireDataAssertionsLane()

	own := s.breachProducer("finance", "f2-own", "")
	foreign := s.breachProducer("marketing", "f2-foreign", "")
	consumerAlias := fmt.Sprintf("f2-consumer-%d", time.Now().UnixNano())
	s.cleanupInOrder(consumerAlias)
	consumer := s.applyAssertionsJob(consumerAlias, holdConsumerManifest(consumerAlias, own.dataset, ""))
	s.Require().Equal("skipped", s.triggerHeldRun(consumer.ID).Status, "the hold stops the consumer until it is released")

	token := s.mintAgentSessionToken(own.incident.ID, own.alias)

	// What the agent sees: the hold, and the series from its own dataset.
	status, raw := s.requestWithKey(http.MethodGet, "/v1/agent/incidents/"+own.incident.ID+"/bundle", token, nil)
	s.Require().Equal(http.StatusOK, status, raw)
	var bundle incident.Bundle
	s.Require().NoError(json.Unmarshal([]byte(raw), &bundle), raw)
	s.Require().NotNil(bundle.DataQuality, raw)
	quality := bundle.DataQuality
	s.Equal(own.holdID, quality.HoldID.String())
	s.Equal(own.dataset, quality.Dataset)
	s.Equal("active", quality.HoldStatus)
	s.Equal(1, quality.OccurrenceCount)
	s.Contains(string(quality.Violations), `"assertion":"min"`)
	s.Require().Len(quality.MetricSeries, 1)
	s.assertRowCountSeries(quality.MetricSeries[0])
	s.Empty(bundle.Failure.LogTail, "the task succeeded: nothing of it may read as the failure")
	s.Nil(bundle.Failure.ExitCode)

	// No parameter reaches another dataset: the route ignores one, MCP refuses it.
	path := "/v1/agent/incidents/" + own.incident.ID + "/context/metrics?metric=rowCount&dataset=" + url.QueryEscape(foreign.dataset)
	status, raw = s.requestWithKey(http.MethodGet, path, token, nil)
	s.Require().Equal(http.StatusOK, status, raw)
	var context struct {
		Series []incident.BundleMetricSeries `json:"series"`
	}
	s.Require().NoError(json.Unmarshal([]byte(raw), &context), raw)
	s.Require().Len(context.Series, 1)
	s.assertRowCountSeries(context.Series[0])

	body := s.callMCPTool(own.incident.ID, token, "get_context", map[string]any{"kind": "metrics"})
	var mcpContext struct {
		Error  *mcpErrorResponse `json:"error"`
		Result struct {
			StructuredContent struct {
				Series []incident.BundleMetricSeries `json:"series"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	s.Require().NoError(json.Unmarshal(body, &mcpContext), string(body))
	s.Require().Nil(mcpContext.Error, string(body))
	s.Require().Len(mcpContext.Result.StructuredContent.Series, 1)
	s.assertRowCountSeries(mcpContext.Result.StructuredContent.Series[0])

	body = s.callMCPTool(own.incident.ID, token, "get_context", map[string]any{"kind": "metrics", "dataset": foreign.dataset})
	s.Contains(string(body), `"error"`, "get_context must refuse a dataset argument: %s", body)

	// Another tenant's hold is refused and nobody is asked to decide it.
	status, refused, raw := s.proposeRelease(own.incident.ID, token, map[string]any{
		"hold_id": foreign.holdID, "reason": "not mine", "tolerate": map[string]string{"min": "24h"},
	})
	s.Require().Equal(http.StatusOK, status, raw)
	s.Equal("failed", refused.Disposition, raw)
	s.Contains(string(refused.Action.Result), "crosses the incident boundary")
	s.NotContains(string(refused.Action.Result), foreign.producer.ID, "the refusal must not name the owner")
	s.Equal("active", s.holdRow(foreign.dataset).Status)

	// The default policy does not let an agent release alone.
	status, denied, raw := s.proposeRelease(own.incident.ID, token, map[string]any{"reason": "looks fine"})
	s.Require().Equal(http.StatusOK, status, raw)
	s.Equal("rejected", denied.Disposition, raw)

	// A tolerance must name what this hold actually breached.
	status, invalid, raw := s.proposeRelease(own.incident.ID, token, map[string]any{
		"reason": "x", "tolerate": map[string]string{"maxLag": "24h"},
	})
	s.Require().Equal(http.StatusOK, status, raw)
	s.Equal("failed", invalid.Disposition, raw)

	detail := s.incidentDetail(own.incident.ID)
	s.Equal("open", detail.Incident.Status)
	s.Empty(detail.Approvals, "none of those reached a human")
	s.Equal("active", s.holdRow(own.dataset).Status)

	// The proposal, through the MCP tool with its unchanged schema. The incident's
	// own hold is the implicit target, so the incident needs no run id for this.
	body = s.callMCPTool(own.incident.ID, token, "propose_action", map[string]any{
		"type":   "release_hold",
		"params": map[string]any{"reason": "month end", "tolerate": map[string]string{"min": "24h"}},
	})
	var proposed struct {
		Error  *mcpErrorResponse `json:"error"`
		Result struct {
			StructuredContent proposedAction `json:"structuredContent"`
		} `json:"result"`
	}
	s.Require().NoError(json.Unmarshal(body, &proposed), string(body))
	s.Require().Nil(proposed.Error, string(body))
	action := proposed.Result.StructuredContent
	s.Require().Equal("awaiting_approval", action.Disposition, string(body))
	s.Require().Equal(3, action.Action.Tier, "a tolerance must end at a human")

	parked := s.incidentDetail(own.incident.ID)
	s.Require().Equal("awaiting_approval", parked.Incident.Status)
	s.Require().Len(parked.Approvals, 1)
	approval := parked.Approvals[0]
	s.Require().Equal(action.Action.ID, approval.ActionID)
	s.Equal("active", s.holdRow(own.dataset).Status, "nothing is released before the decision")

	// The agent cannot decide its own proposal.
	approveURL := fmt.Sprintf("%s/v1/incidents/%s/approvals/%s/approve", s.caesiumURL, own.incident.ID, approval.ID)
	status, body = s.postWithToken(approveURL, token, nil)
	s.Require().Equal(http.StatusForbidden, status, string(body))

	out, err := s.runCLIStdout(s.incidentCLIArgs("approve", own.incident.ID, "--approval", approval.ID, "--reason", "seasonal")...)
	s.Require().NoError(err, "caesium incident approve failed:\n%s", out)
	s.Require().True(json.Valid([]byte(out)), "stdout was not clean JSON:\n%s", out)

	// The release committed inside the approve; the close is the subscriber's.
	closed := s.awaitIncidentStatus(own.incident.ID, "closed", 60*time.Second)
	var decider string
	for _, row := range closed.Approvals {
		if row.ID == approval.ID {
			s.Equal("approved", row.Decision)
			decider = row.Decider
		}
	}
	s.Require().NotEmpty(decider)
	var executed *approvalAction
	for i := range closed.Actions {
		if closed.Actions[i].ID == action.Action.ID {
			executed = &closed.Actions[i]
		}
	}
	s.Require().NotNil(executed)
	s.Equal("executed", executed.Status)
	s.Contains(string(executed.Result), "not enforced", "the record must say what a tolerance does")

	hold := s.holdRow(own.dataset)
	s.Equal(own.holdID, hold.ID)
	s.Equal("released", hold.Status)
	s.Equal(decider, hold.ReleasedBy)
	s.Equal("manual_ack", hold.ReleaseReason, "one release path, so one release reason")
	s.Equal("month end", hold.ReleaseNote)
	s.JSONEq(`{"min":"24h"}`, string(hold.Tolerances))
	final := s.holdIncidents(own.producer.ID)
	s.Require().Len(final, 1)
	s.Contains(final[0].ResolutionSummary, "manual_ack by "+decider)

	var audits []models.AuditLog
	s.Require().NoError(s.openIntegrationCatalogGorm().
		Where("action = ? AND resource_id = ?", "dataset.hold.release", own.holdID).Find(&audits).Error)
	s.Require().Len(audits, 1, "one release, one audit row of the endpoint's kind")
	s.Equal("success", audits[0].Outcome)
	s.Equal(decider, audits[0].Actor)
	s.Equal("finance", audits[0].Namespace)

	// The gate reopened, and the foreign hold never moved.
	runID := s.triggerRun(consumer.ID)
	s.Equal("succeeded", s.awaitRun(consumer.ID, runID, runTimeout).Status)
	s.Equal("active", s.holdRow(foreign.dataset).Status)

	// A decision is once-only, and the action ran once.
	status, body = s.postWithToken(approveURL, s.authAPIKey, nil)
	s.Equal(http.StatusConflict, status, string(body))
}

func (s *IntegrationTestSuite) assertRowCountSeries(series incident.BundleMetricSeries) {
	s.T().Helper()
	s.Equal("rowCount", series.Metric)
	s.Require().Len(series.Samples, 2, "one clean sample and the breach, and nothing else's")
	s.InDelta(5000, series.Samples[0].Value, 0.001)
	s.False(series.Samples[0].Violated)
	s.InDelta(12, series.Samples[1].Value, 0.001)
	s.True(series.Samples[1].Violated)
}

// TestHoldReleaseActionRunsAloneUnderAnExplicitAllow is the tier-2 road: a job whose
// playbook names release_hold lets its agent release the hold with no approval, as
// that session and through the same service, and a tolerance still stops at a human.
func (s *IntegrationTestSuite) TestHoldReleaseActionRunsAloneUnderAnExplicitAllow() {
	s.requireAuthLane()
	s.requirePolicyFixture()
	s.requireDataAssertionsLane()

	remediation := `
  remediation:
    profile: triage-only
    classes: [data_quality_hold]
    autonomy:
      allow: [release_hold]`
	f := s.breachProducer("finance", "f2-alone", remediation)
	token := s.mintAgentSessionToken(f.incident.ID, f.alias)

	status, gated, raw := s.proposeRelease(f.incident.ID, token, map[string]any{
		"reason": "x", "tolerate": map[string]string{"min": "1h"},
	})
	s.Require().Equal(http.StatusAccepted, status, raw)
	s.Equal("awaiting_approval", gated.Disposition, "the allow does not reach a tolerance: %s", raw)
	s.Equal("active", s.holdRow(f.dataset).Status)

	status, ran, raw := s.proposeRelease(f.incident.ID, token, map[string]any{"reason": "source backfilled"})
	s.Require().Equal(http.StatusOK, status, raw)
	s.Require().Equal("executed", ran.Disposition, raw)
	s.Equal(2, ran.Action.Tier)

	hold := s.holdRow(f.dataset)
	s.Equal("released", hold.Status)
	s.Contains(hold.ReleasedBy, "agent:", "the release names the agent, not a human")
	s.Equal("source backfilled", hold.ReleaseNote)
	s.awaitIncidentStatus(f.incident.ID, "closed", 60*time.Second)

	// The pending approval still answers: its hold has ended, so nothing happens.
	pending := s.incidentDetail(f.incident.ID)
	s.Require().Len(pending.Approvals, 1)
	out, err := s.runCLIStdout(s.incidentCLIArgs("approve", f.incident.ID, "--approval", pending.Approvals[0].ID, "--reason", "late")...)
	s.Require().NoError(err, "caesium incident approve failed:\n%s", out)
	late := s.incidentDetail(f.incident.ID)
	s.Equal("closed", late.Incident.Status)
	for _, row := range late.Actions {
		if row.ID == gated.Action.ID {
			s.Equal("executed", row.Status)
			s.Contains(string(row.Result), `"already_released":true`)
		}
	}
	s.Equal(hold.ReleasedBy, s.holdRow(f.dataset).ReleasedBy, "the late approval must not re-release")
}
