//go:build integration

package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/connector"
	ctemporal "github.com/caesium-cloud/caesium/internal/connector/temporal"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	querypb "go.temporal.io/api/query/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
)

type messageFixtureRef struct {
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id"`
}

type messageFixtureState struct {
	Stage           string                   `json:"stage"`
	Eligible        bool                     `json:"eligible"`
	Approved        bool                     `json:"approved"`
	ApprovalEffects int                      `json:"approval_effects"`
	Actor           *connector.ActorEnvelope `json:"actor"`
}

type messageFixtureEvent struct {
	Kind        string                   `json:"kind"`
	RunID       string                   `json:"run_id"`
	UpdateID    string                   `json:"update_id"`
	RPCCode     string                   `json:"rpc_code"`
	Stage       string                   `json:"stage"`
	Rejected    bool                     `json:"rejected"`
	ReplyLost   bool                     `json:"reply_dropped"`
	Actor       *connector.ActorEnvelope `json:"actor,omitempty"`
	WorkflowID  string                   `json:"response_workflow_id"`
	ResponseID  string                   `json:"response_update_id"`
	ResponseRun string                   `json:"response_run_id"`
}

// Own helpers intentionally do not depend on F1's Go test source: this tagged
// test compiles in B2 before F1 merges, but selecting it requires F1's actual
// worker, mTLS transport, configuration and independent effect journal.
func (s *IntegrationTestSuite) messageFixtureControl(method, path string, input, output any) {
	s.T().Helper()
	var raw []byte
	var err error
	if input != nil {
		raw, err = json.Marshal(input)
		s.Require().NoError(err)
	}
	ctx, cancel := context.WithTimeout(s.T().Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://127.0.0.1:7240"+path, bytes.NewReader(raw))
	s.Require().NoError(err)
	req.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	s.Require().NoError(err, "selected connector message scenario requires the real fixture control")
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, response.StatusCode, "fixture control %s returned %s", path, body)
	if output != nil {
		s.Require().NoError(json.Unmarshal(body, output))
	}
}

func (s *IntegrationTestSuite) messageFixtureStart(c client.Client, label, workflowType string, eligible bool) messageFixtureRef {
	s.T().Helper()
	var ref messageFixtureRef
	s.messageFixtureControl(http.MethodPost, "/start", map[string]any{
		"id": fmt.Sprintf("message-%s-%d", label, time.Now().UnixNano()), "type": workflowType,
		"input": map[string]any{"message_only": true, "eligible": eligible},
	}, &ref)
	s.Require().NotEmpty(ref.WorkflowID)
	s.Require().NotEmpty(ref.RunID)
	s.T().Cleanup(func() {
		// This execution belongs only to this isolated fixture scenario. A
		// waiting approval does not consume finish signals until approved.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = c.TerminateWorkflow(ctx, ref.WorkflowID, ref.RunID, "connector message fixture cleanup")
	})
	return ref
}

func messageFixtureTarget(connection connector.Connection, ref messageFixtureRef) ctemporal.MessageTarget {
	return ctemporal.MessageTarget{ConnectionID: connection.ID, WorkflowID: ref.WorkflowID, RunID: ref.RunID, BindingName: "publication", BindingVersion: "1"}
}

func (s *IntegrationTestSuite) messageFixtureActor(key createdAPIKey, operation string) connector.ActorEnvelope {
	s.T().Helper()
	code, body := s.requestWithKey(http.MethodGet, "/auth/whoami", key.Plaintext, nil)
	s.Require().Equal(http.StatusOK, code, "selected messages scenario requires real authenticated whoami")
	var principal struct {
		Kind    connector.PrincipalKind `json:"kind"`
		Subject string                  `json:"subject"`
		Role    string                  `json:"role"`
	}
	s.Require().NoError(json.Unmarshal([]byte(body), &principal))
	s.Require().Equal(connector.PrincipalKindAPIKey, principal.Kind)
	s.Require().Equal(key.Meta.KeyPrefix, principal.Subject)
	s.Require().Equal("operator", principal.Role)
	s.Require().Nil(key.Meta.Scope, "fixture operator must be unscoped")
	actor, err := connector.NewActorEnvelope(principal.Kind, key.Meta.ID, principal.Subject, principal.Role, operation, "1")
	s.Require().NoError(err)
	return actor
}

func (s *IntegrationTestSuite) messageFixtureAwait(m *ctemporal.Messenger, target ctemporal.MessageTarget, ready func(messageFixtureState) bool) messageFixtureState {
	s.T().Helper()
	var state messageFixtureState
	var lastErr error
	s.Require().Eventually(func() bool {
		var raw json.RawMessage
		raw, lastErr = m.QueryStatus(s.T().Context(), target)
		if lastErr != nil {
			return false
		}
		lastErr = json.Unmarshal(raw, &state)
		return lastErr == nil && ready(state)
	}, 30*time.Second, 200*time.Millisecond, "actual message worker did not reach expected state; last=%+v error=%v", state, lastErr)
	return state
}

func (s *IntegrationTestSuite) messageFixtureCompleted(m *ctemporal.Messenger, target ctemporal.MessageTarget, id string) ctemporal.UpdateResult {
	s.T().Helper()
	var result ctemporal.UpdateResult
	var lastErr error
	s.Require().Eventually(func() bool {
		result, lastErr = m.LookupAction(s.T().Context(), target, "approve_publication", id)
		return lastErr == nil && result.State == ctemporal.UpdateCompleted
	}, 20*time.Second, 200*time.Millisecond, "same-ID lookup did not complete: %+v %v", result, lastErr)
	var decoded struct {
		Approved bool `json:"approved"`
	}
	s.Require().NoError(json.Unmarshal(result.Result, &decoded))
	s.Require().True(decoded.Approved)
	return result
}

func (s *IntegrationTestSuite) messageFixtureEvents() []messageFixtureEvent {
	s.T().Helper()
	var events []messageFixtureEvent
	s.messageFixtureControl(http.MethodGet, "/evidence", nil, &events)
	return events
}

func messageFixtureCount(events []messageFixtureEvent, kind, updateID, runID string) int {
	n := 0
	for _, event := range events {
		if event.Kind == kind && (updateID == "" || event.UpdateID == updateID) && (runID == "" || event.RunID == runID) {
			n++
		}
	}
	return n
}

// TestConnectorsMessages qualifies B2 against F1's pinned real Temporal server.
// It supplements adapter tests without claiming C2's future HTTP action surface.
func (s *IntegrationTestSuite) TestConnectorsMessages() {
	if os.Getenv("CAESIUM_CONNECTORS_FIXTURE") != "true" {
		s.T().Skip("requires explicitly selected real connector fixture")
	}
	s.Require().NotEmpty(s.authAPIKey, "selected connector messages require the authenticated Caesium server")
	artifacts := os.Getenv("CAESIUM_CONNECTORS_ARTIFACTS")
	s.Require().NotEmpty(artifacts, "selected messages require preserved fixture artifacts")
	info, err := os.Stat(artifacts)
	s.Require().NoError(err)
	s.Require().True(info.IsDir())
	candidate := os.Getenv("CAESIUM_CONNECTORS_CANDIDATE_SHA")
	s.Require().Len(candidate, 40, "selected messages require candidate-bound evidence")
	var readiness map[string]any
	s.messageFixtureControl(http.MethodGet, "/health", nil, &readiness)
	s.Require().Equal(true, readiness["ready"], "Temporal server alone is not a ready fixture")
	s.Require().Equal("1.49.0", readiness["sdk"])
	s.Require().Equal("1.32.0", readiness["server"])
	configPath := os.Getenv("CAESIUM_CONNECTORS_FIXTURE_CONFIG")
	if configPath == "" {
		configPath = filepath.Join(s.projectRoot, "test", "connectors", "testdata", "connections.yaml")
	}
	configBytes, err := os.ReadFile(configPath)
	s.Require().NoError(err, "selected messages require F1's strict configuration")
	config, err := connector.Parse(configBytes, nil)
	s.Require().NoError(err)
	s.Require().Len(config.Connections, 1)
	connection := config.Connections[0]
	s.Require().True(connection.Enabled)
	s.Require().Equal(connector.ProviderTemporal, connection.Provider)
	s.Require().Equal("127.0.0.1:7234", connection.Endpoint)
	s.Require().Equal("default", connection.Scope)
	s.Require().Len(connection.CertificatePaths, 3)
	for _, certificate := range connection.CertificatePaths {
		certInfo, err := os.Stat(certificate)
		s.Require().NoError(err, "selected messages require real mTLS artifacts")
		s.Require().True(certInfo.Mode().IsRegular())
	}
	ctx, cancel := context.WithTimeout(s.T().Context(), 5*time.Second)
	c, err := ctemporal.Dial(ctx, ctemporal.DialConfig{Endpoint: connection.Endpoint, Namespace: connection.Scope, CertificatePaths: connection.CertificatePaths})
	cancel()
	s.Require().NoError(err)
	s.T().Cleanup(c.Close)
	m, err := ctemporal.NewMessengerFromClient(c, connection.ID, connection.Scope, connection.Bindings, "", 3*time.Second, connection.Limits.MaxPageMetadataBytes)
	s.Require().NoError(err)
	operator := s.createAPIKeyCLI("--role", "operator", "--description", "connector messages fixture operator")
	operationPrefix := fmt.Sprintf("fixture-message-operation-%d", time.Now().UnixNano())
	actor := s.messageFixtureActor(operator, operationPrefix+"-approved")
	updateID, err := ctemporal.StableUpdateID(actor.OperationID)
	s.Require().NoError(err)
	ref := s.messageFixtureStart(c, "approved", "publication", true)
	target := messageFixtureTarget(connection, ref)
	s.messageFixtureAwait(m, target, func(state messageFixtureState) bool { return state.Stage == "awaiting_approval" })
	input := map[string]any{"note": "real actor approval"}
	beforeRefusals := messageFixtureCount(s.messageFixtureEvents(), "update_rpc", "", "")
	for _, rejectedInput := range []map[string]any{{"note": 42}, {connector.ReservedActorField: actor}} {
		_, err := m.SubmitAction(s.T().Context(), target, "approve_publication", updateID, rejectedInput, actor)
		s.Require().Error(err)
	}
	for _, mutate := range []func(*ctemporal.MessageTarget){
		func(target *ctemporal.MessageTarget) { target.RunID = "" },
		func(target *ctemporal.MessageTarget) { target.RunID = "another-run" },
		func(target *ctemporal.MessageTarget) { target.BindingVersion = "changed" },
	} {
		bad := target
		mutate(&bad)
		_, err := m.SubmitAction(s.T().Context(), bad, "approve_publication", updateID, input, actor)
		s.Require().Error(err)
	}
	_, err = m.SubmitAction(s.T().Context(), target, "undeclared_update", updateID, input, actor)
	s.Require().Error(err)
	external := s.messageFixtureStart(c, "type-refusal", "external_only", true)
	_, err = m.SubmitAction(s.T().Context(), messageFixtureTarget(connection, external), "approve_publication", updateID, input, actor)
	s.Require().Error(err, "actual workflow type mismatch must be refused")
	s.Require().Equal(beforeRefusals, messageFixtureCount(s.messageFixtureEvents(), "update_rpc", "", ""), "schema/spoof/handler/target refusals must happen before an Update RPC")

	// The special test-only Query returns a real unsupported encoding. The
	// production adapter must refuse it without an SDK custom-converter path.
	opaqueBindings := append([]connector.Binding(nil), connection.Bindings...)
	opaqueBindings[0].StatusQuery = "fixture_opaque_status"
	opaque, err := ctemporal.NewMessengerFromClient(c, connection.ID, connection.Scope, opaqueBindings, "", 3*time.Second, 0)
	s.Require().NoError(err)
	opaqueCtx, opaqueCancel := context.WithTimeout(s.T().Context(), 3*time.Second)
	opaqueWire, err := c.WorkflowService().QueryWorkflow(opaqueCtx, &workflowservice.QueryWorkflowRequest{
		Namespace: connection.Scope,
		Execution: &commonpb.WorkflowExecution{WorkflowId: target.WorkflowID, RunId: target.RunID},
		Query:     &querypb.WorkflowQuery{QueryType: "fixture_opaque_status"},
	})
	opaqueCancel()
	s.Require().NoError(err, "custom-result refusal requires a successful real handler RPC")
	s.Require().Len(opaqueWire.GetQueryResult().GetPayloads(), 1)
	s.Require().Equal("binary/fixture", string(opaqueWire.GetQueryResult().GetPayloads()[0].GetMetadata()["encoding"]))
	opaqueResult, err := opaque.QueryStatus(s.T().Context(), target)
	s.Require().Error(err)
	s.Require().Empty(opaqueResult, "custom payloads must remain metadata-only")

	accepted, err := m.SubmitAction(s.T().Context(), target, "approve_publication", updateID, input, actor)
	s.Require().NoError(err)
	s.Require().Contains([]string{ctemporal.UpdateAccepted, ctemporal.UpdateCompleted}, accepted.State)
	s.Require().Equal(1, messageFixtureCount(s.messageFixtureEvents(), "update_rpc", updateID, ""), "one explicit submission must issue exactly one Update RPC")
	s.messageFixtureCompleted(m, target, updateID)
	state := s.messageFixtureAwait(m, target, func(state messageFixtureState) bool { return state.Stage == "published" })
	s.Require().Equal(&actor, state.Actor, "workflow-visible principal must come from authenticated Caesium whoami")
	s.Require().Equal(1, state.ApprovalEffects)
	s.Require().Equal(1, messageFixtureCount(s.messageFixtureEvents(), "approval_effect", "", ref.RunID))
	// Verify the immutable actor also reached actual accepted Update history.
	iterator := c.GetWorkflowHistory(s.T().Context(), ref.WorkflowID, ref.RunID, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	actorFound := false
	for iterator.HasNext() {
		event, err := iterator.Next()
		s.Require().NoError(err)
		if acceptedEvent := event.GetWorkflowExecutionUpdateAcceptedEventAttributes(); acceptedEvent != nil && acceptedEvent.GetAcceptedRequest().GetMeta().GetUpdateId() == updateID {
			var envelope struct {
				Actor connector.ActorEnvelope `json:"_caesium_actor"`
			}
			s.Require().NoError(converter.GetDefaultDataConverter().FromPayloads(acceptedEvent.GetAcceptedRequest().GetInput().GetArgs(), &envelope))
			s.Require().Equal(actor, envelope.Actor)
			actorFound = true
		}
	}
	s.Require().True(actorFound, "actor assertion needs actual accepted Update history")

	// A lost accepted reply resolves by the same id/run, including through a
	// freshly constructed adapter. Lookup must not send another Update.
	lostAccepted := s.messageFixtureStart(c, "lost-accepted", "publication", true)
	lostTarget := messageFixtureTarget(connection, lostAccepted)
	s.messageFixtureAwait(m, lostTarget, func(state messageFixtureState) bool { return state.Stage == "awaiting_approval" })
	lostActor := s.messageFixtureActor(operator, operationPrefix+"-lost-accepted")
	lostID, err := ctemporal.StableUpdateID(lostActor.OperationID)
	s.Require().NoError(err)
	s.messageFixtureControl(http.MethodPost, "/fault", map[string]any{"update_id": lostID, "mode": "accepted"}, nil)
	lostResult, err := m.SubmitAction(s.T().Context(), lostTarget, "approve_publication", lostID, input, lostActor)
	s.Require().Error(err)
	s.Require().Equal(ctemporal.UpdateUnknown, lostResult.State)
	s.Require().Equal("DeadlineExceeded", lostResult.RPCCode)
	s.Require().Equal(1, messageFixtureCount(s.messageFixtureEvents(), "update_rpc", lostID, ""), "lost accepted reply must not trigger an implicit resend")
	s.messageFixtureAwait(m, lostTarget, func(state messageFixtureState) bool { return state.Stage == "published" })
	acceptedSends := messageFixtureCount(s.messageFixtureEvents(), "update_rpc", lostID, "")
	restarted, err := ctemporal.NewMessengerFromClient(c, connection.ID, connection.Scope, connection.Bindings, "", 3*time.Second, 0)
	s.Require().NoError(err)
	s.messageFixtureCompleted(restarted, lostTarget, lostID)
	s.Require().Equal(acceptedSends, messageFixtureCount(s.messageFixtureEvents(), "update_rpc", lostID, ""))
	s.Require().Equal(1, messageFixtureCount(s.messageFixtureEvents(), "approval_effect", "", lostAccepted.RunID))
	s.messageFixtureControl(http.MethodPost, "/fault", map[string]any{}, nil)

	// Direct validator failure is known rejection. A dropped rejection is
	// unknown; NotFound after eligibility changes must never resend it.
	rejected := s.messageFixtureStart(c, "rejected", "publication", false)
	rejectedTarget := messageFixtureTarget(connection, rejected)
	s.messageFixtureAwait(m, rejectedTarget, func(state messageFixtureState) bool { return state.Stage == "awaiting_approval" })
	rejectedActor := s.messageFixtureActor(operator, operationPrefix+"-rejected")
	rejectedID, err := ctemporal.StableUpdateID(rejectedActor.OperationID)
	s.Require().NoError(err)
	knownRejection, err := m.SubmitAction(s.T().Context(), rejectedTarget, "approve_publication", rejectedID, input, rejectedActor)
	s.Require().NoError(err)
	s.Require().Equal(ctemporal.UpdateRejected, knownRejection.State)
	s.Require().Equal("remote_update_failure", knownRejection.FailureKind)
	s.Require().Equal(1, messageFixtureCount(s.messageFixtureEvents(), "update_rpc", rejectedID, ""), "known rejection must come from one explicit Update RPC")
	lostRejectedActor := s.messageFixtureActor(operator, operationPrefix+"-lost-rejected")
	lostRejectedID, err := ctemporal.StableUpdateID(lostRejectedActor.OperationID)
	s.Require().NoError(err)
	s.messageFixtureControl(http.MethodPost, "/fault", map[string]any{"update_id": lostRejectedID, "mode": "rejected"}, nil)
	lostRejection, err := m.SubmitAction(s.T().Context(), rejectedTarget, "approve_publication", lostRejectedID, input, lostRejectedActor)
	s.Require().Error(err)
	s.Require().Equal(ctemporal.UpdateUnknown, lostRejection.State)
	s.Require().Equal("DeadlineExceeded", lostRejection.RPCCode)
	s.Require().Equal(1, messageFixtureCount(s.messageFixtureEvents(), "update_rpc", lostRejectedID, ""), "lost rejected reply must not trigger an implicit resend")
	unknown, err := m.LookupAction(s.T().Context(), rejectedTarget, "approve_publication", lostRejectedID)
	s.Require().Error(err)
	s.Require().Equal(ctemporal.UpdateUnknown, unknown.State)
	s.Require().Equal("NotFound", unknown.RPCCode)
	rejectedSends := messageFixtureCount(s.messageFixtureEvents(), "update_rpc", lostRejectedID, "")
	s.messageFixtureControl(http.MethodPost, "/signal", map[string]any{"id": rejected.WorkflowID, "run_id": rejected.RunID, "name": "fixture_eligibility", "value": true}, nil)
	s.messageFixtureAwait(m, rejectedTarget, func(state messageFixtureState) bool { return state.Eligible })
	for range 2 {
		unknown, err = restarted.LookupAction(s.T().Context(), rejectedTarget, "approve_publication", lostRejectedID)
		s.Require().Error(err)
		s.Require().Equal(ctemporal.UpdateUnknown, unknown.State)
		s.Require().Equal("NotFound", unknown.RPCCode)
	}
	state = s.messageFixtureAwait(m, rejectedTarget, func(state messageFixtureState) bool { return state.Eligible && state.Stage == "awaiting_approval" })
	s.Require().False(state.Approved)
	s.Require().Zero(state.ApprovalEffects)
	s.Require().Equal(rejectedSends, messageFixtureCount(s.messageFixtureEvents(), "update_rpc", lostRejectedID, ""))
	s.Require().Zero(messageFixtureCount(s.messageFixtureEvents(), "approval_effect", "", rejected.RunID))
	s.messageFixtureControl(http.MethodPost, "/fault", map[string]any{}, nil)

	events := s.messageFixtureEvents()
	var provedAcceptedLoss, provedRejectedLoss, provedRejection bool
	for _, event := range events {
		if event.Kind != "update_rpc" {
			continue
		}
		if event.UpdateID == lostID && event.ReplyLost && !event.Rejected && event.RPCCode == "OK" {
			provedAcceptedLoss = true
		}
		if event.UpdateID == lostRejectedID && event.ReplyLost && event.Rejected && event.RPCCode == "OK" {
			provedRejectedLoss = true
		}
		if event.UpdateID == rejectedID && event.Rejected && event.RPCCode == "OK" {
			provedRejection = true
		}
	}
	s.Require().True(provedAcceptedLoss, "fault must drop a real accepted response")
	s.Require().True(provedRejectedLoss, "fault must drop a real validator rejection response")
	s.Require().True(provedRejection, "known rejection must have provider outcome evidence")
	evidence, err := json.MarshalIndent(map[string]any{"candidate_sha": candidate, "sdk": "1.49.0", "server": "1.32.0", "events": events}, "", "  ")
	s.Require().NoError(err)
	s.Require().NotContains(string(evidence), operator.Plaintext)
	s.Require().NoError(os.WriteFile(filepath.Join(artifacts, "messages-provider-evidence.json"), append(evidence, '\n'), 0600))
	s.Require().True(strings.HasPrefix(updateID, "caesium-"))
}
