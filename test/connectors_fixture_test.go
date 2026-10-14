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
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/serviceerror"
	updatepb "go.temporal.io/api/update/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
)

type fixtureRef struct {
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id"`
}
type fixtureState struct {
	Stage           string                   `json:"stage"`
	Eligible        bool                     `json:"eligible"`
	Approved        bool                     `json:"approved"`
	ApprovalEffects int                      `json:"approval_effects"`
	Actor           *connector.ActorEnvelope `json:"actor"`
	First, Second   *connector.CorrelationEnvelope
	ChildRunID      string `json:"child_run_id"`
}

func (s *IntegrationTestSuite) fixtureRequest(method, path string, in, out any) int {
	s.T().Helper()
	var body []byte
	var err error
	if in != nil {
		body, err = json.Marshal(in)
		s.Require().NoError(err)
	}
	req, err := http.NewRequestWithContext(s.T().Context(), method, "http://127.0.0.1:7240"+path, bytes.NewReader(body))
	s.Require().NoError(err)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	if out != nil {
		s.Require().Equal(http.StatusOK, resp.StatusCode, string(raw))
		s.Require().NoError(json.Unmarshal(raw, out))
	}
	return resp.StatusCode
}
func (s *IntegrationTestSuite) fixtureStart(label, workflowType, first, second string, eligible, retry, opaque, child bool) fixtureRef {
	s.T().Helper()
	var ref fixtureRef
	s.fixtureRequest(http.MethodPost, "/start", map[string]any{"id": fmt.Sprintf("fixture-%s-%d", label, time.Now().UnixNano()), "type": workflowType, "input": map[string]any{"first_job": first, "second_job": second, "eligible": eligible, "retry_first": retry, "metadata_only": opaque, "child": child}}, &ref)
	s.Require().NotEmpty(ref.WorkflowID)
	s.Require().NotEmpty(ref.RunID)
	s.T().Cleanup(func() {
		s.fixtureRequest(http.MethodPost, "/signal", map[string]any{"id": ref.WorkflowID, "name": "fixture_finish", "value": true}, nil)
	})
	return ref
}
func (s *IntegrationTestSuite) fixtureSignal(ref fixtureRef, name string, value bool) {
	s.T().Helper()
	s.Require().Equal(http.StatusOK, s.fixtureRequest(http.MethodPost, "/signal", map[string]any{"id": ref.WorkflowID, "run_id": ref.RunID, "name": name, "value": value}, nil))
}
func (s *IntegrationTestSuite) fixtureQuery(c client.Client, ref fixtureRef) (fixtureState, error) {
	ctx, cancel := context.WithTimeout(s.T().Context(), 2*time.Second)
	defer cancel()
	value, err := c.QueryWorkflow(ctx, ref.WorkflowID, ref.RunID, "publication_status")
	if err != nil {
		return fixtureState{}, err
	}
	var state fixtureState
	err = value.Get(&state)
	return state, err
}
func (s *IntegrationTestSuite) fixtureAwait(c client.Client, ref fixtureRef, stage string) fixtureState {
	s.T().Helper()
	var state fixtureState
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		state, err = s.fixtureQuery(c, ref)
		if err == nil && state.Stage == stage {
			return state
		}
		time.Sleep(250 * time.Millisecond)
	}
	s.T().Fatalf("real worker did not reach %s: %+v", stage, state)
	return state
}
func (s *IntegrationTestSuite) fixtureUpdate(c client.Client, ref fixtureRef, id string, actor connector.ActorEnvelope) (*workflowservice.UpdateWorkflowExecutionResponse, error) {
	payload, err := converter.GetDefaultDataConverter().ToPayloads(map[string]any{"input": map[string]string{"note": "fixture approval"}, "_caesium_actor": actor})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(s.T().Context(), 3*time.Second)
	defer cancel()
	return c.WorkflowService().UpdateWorkflowExecution(ctx, &workflowservice.UpdateWorkflowExecutionRequest{Namespace: "default", WorkflowExecution: &commonpb.WorkflowExecution{WorkflowId: ref.WorkflowID, RunId: ref.RunID}, Request: &updatepb.Request{Meta: &updatepb.Meta{UpdateId: id, Identity: "caesium-fixture-trusted-producer"}, Input: &updatepb.Input{Name: "approve_publication", Args: payload}}, WaitPolicy: &updatepb.WaitPolicy{LifecycleStage: enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED}})
}
func (s *IntegrationTestSuite) fixtureLookup(c client.Client, ref fixtureRef, id string) (*workflowservice.PollWorkflowExecutionUpdateResponse, error) {
	ctx, cancel := context.WithTimeout(s.T().Context(), 3*time.Second)
	defer cancel()
	return c.WorkflowService().PollWorkflowExecutionUpdate(ctx, &workflowservice.PollWorkflowExecutionUpdateRequest{Namespace: "default", UpdateRef: &updatepb.UpdateRef{WorkflowExecution: &commonpb.WorkflowExecution{WorkflowId: ref.WorkflowID, RunId: ref.RunID}, UpdateId: id}})
}
func (s *IntegrationTestSuite) fixtureHistory(c client.Client, ref fixtureRef) []*historypb.HistoryEvent {
	s.T().Helper()
	it := c.GetWorkflowHistory(s.T().Context(), ref.WorkflowID, ref.RunID, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	var events []*historypb.HistoryEvent
	for it.HasNext() {
		event, err := it.Next()
		s.Require().NoError(err)
		events = append(events, event)
	}
	raw, err := protojson.MarshalOptions{Indent: "  "}.Marshal(&historypb.History{Events: events})
	s.Require().NoError(err)
	s.Require().NoError(os.WriteFile(filepath.Join(os.Getenv("CAESIUM_CONNECTORS_ARTIFACTS"), ref.RunID+".history.json"), raw, 0644))
	return events
}
func (s *IntegrationTestSuite) fixtureEvidence() []map[string]any {
	var events []map[string]any
	s.fixtureRequest(http.MethodGet, "/evidence", nil, &events)
	return events
}
func fixtureCount(events []map[string]any, kind, key, value string) int {
	n := 0
	for _, event := range events {
		if event["kind"] == kind && event[key] == value {
			n++
		}
	}
	return n
}

func (s *IntegrationTestSuite) fixtureActor(key createdAPIKey, operation string) connector.ActorEnvelope {
	s.T().Helper()
	code, body := s.requestWithKey(http.MethodGet, "/auth/whoami", key.Plaintext, nil)
	s.Require().Equal(http.StatusOK, code)
	var principal struct {
		Kind    connector.PrincipalKind `json:"kind"`
		Subject string                  `json:"subject"`
		Role    string                  `json:"role"`
	}
	s.Require().NoError(json.Unmarshal([]byte(body), &principal))
	s.Require().Equal(connector.PrincipalKindAPIKey, principal.Kind)
	s.Require().Equal(key.Meta.KeyPrefix, principal.Subject)
	s.Require().Equal(key.Meta.Role, principal.Role)
	actor, err := connector.NewActorEnvelope(principal.Kind, key.Meta.ID, principal.Subject, principal.Role, operation, "1")
	s.Require().NoError(err)
	return actor
}

func fixtureEffectActivities(events []map[string]any, runID string) int {
	distinct := map[string]bool{}
	for _, event := range events {
		if event["kind"] == "effect_attempt" && event["run_id"] == runID {
			if id, ok := event["activity_id"].(string); ok {
				distinct[id] = true
			}
		}
	}
	return len(distinct)
}

// F1 is selected explicitly by connectors-fixture-check. Outside that lane it
// remains integration-tagged and skipped; selected absence fails readiness.
func (s *IntegrationTestSuite) TestConnectorsFixture() {
	if os.Getenv("CAESIUM_CONNECTORS_FIXTURE") != "true" {
		s.T().Skip("requires selected real connector fixture")
	}
	artifacts := os.Getenv("CAESIUM_CONNECTORS_ARTIFACTS")
	s.Require().NotEmpty(artifacts)
	candidate := os.Getenv("CAESIUM_CONNECTORS_CANDIDATE_SHA")
	s.Require().Len(candidate, 40)
	var ready map[string]any
	s.fixtureRequest(http.MethodGet, "/health", nil, &ready)
	s.Require().Equal(true, ready["ready"])
	s.Require().Equal("1.49.0", ready["sdk"])
	certs := []string{filepath.Join(artifacts, "tls/ca.crt"), filepath.Join(artifacts, "tls/tls.crt"), filepath.Join(artifacts, "tls/tls.key")}
	ctx, cancel := context.WithTimeout(s.T().Context(), 5*time.Second)
	defer cancel()
	c, err := ctemporal.Dial(ctx, ctemporal.DialConfig{Endpoint: "127.0.0.1:7234", Namespace: "default", CertificatePaths: certs})
	s.Require().NoError(err)
	defer c.Close()
	// CA verification and client identity both execute on the real transport.
	negativeCtx, negativeCancel := context.WithTimeout(s.T().Context(), 2*time.Second)
	negative, err := ctemporal.Dial(negativeCtx, ctemporal.DialConfig{Endpoint: "127.0.0.1:7234", Namespace: "default", CertificatePaths: certs[:1]})
	negativeCancel()
	if negative != nil {
		negative.Close()
	}
	s.Require().Error(err, "mTLS endpoint must reject a missing client certificate")
	system, err := c.WorkflowService().GetSystemInfo(s.T().Context(), &workflowservice.GetSystemInfoRequest{})
	s.Require().NoError(err)
	s.Require().Equal("1.32.0", system.GetServerVersion(), "pinned CLI must run the qualified server")
	s.Require().NoError(os.WriteFile(filepath.Join(artifacts, "server-version.txt"), []byte(system.GetServerVersion()+"\n"), 0644))
	observer, err := ctemporal.NewObserverFromClient(c, "default", "", 3*time.Second, 100, 65536)
	s.Require().NoError(err)

	// Use the real container-built CLI to register both jobs. No app rows seed
	// expected admissions, runs, relationships, or operation outcomes.
	s.Require().NotEmpty(s.authAPIKey, "selected fixture requires the tracked authenticated server bootstrap")
	jobs := make([]string, 2)
	aliases := make([]string, 2)
	for i := range jobs {
		alias := fmt.Sprintf("connector-fixture-%d-%d", i, time.Now().UnixNano())
		manifest := fmt.Sprintf("apiVersion: v1\nkind: Job\nmetadata:\n  alias: %s\ntrigger:\n  type: cron\n  configuration:\n    cron: '0 0 1 1 *'\nsteps:\n  - name: work\n    image: alpine:3.23\n    command: [sh, -c, 'sleep 1; echo fixture-work']\n", alias)
		dir := s.writeJobManifest(manifest)
		defer os.RemoveAll(dir)
		s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
		jobs[i] = s.requireJobByAlias(alias).ID
		aliases[i] = alias
	}
	runnerKey := s.createAPIKeyCLI("--role", "runner", "--scope-jobs", strings.Join(aliases, ","), "--description", "connector fixture runner")
	s.Require().Equal(http.StatusOK, s.fixtureRequest(http.MethodPost, "/configure", map[string]string{"runner_key": runnerKey.Plaintext}, nil))
	operatorKey := s.createAPIKeyCLI("--role", "operator", "--description", "connector fixture operator")
	actor := s.fixtureActor(operatorKey, "operation-accepted")
	external := s.fixtureStart("external", "external_only", "", "", true, false, false, false)
	described, err := observer.Describe(s.T().Context(), external.WorkflowID, external.RunID)
	s.Require().NoError(err)
	s.Require().Equal(external.RunID, described.RunID)
	s.Require().False(described.Terminal)

	publication := s.fixtureStart("publication", "publication", jobs[0], jobs[1], true, true, false, true)
	state := s.fixtureAwait(c, publication, "awaiting_approval")
	s.Require().NotNil(state.First)
	s.Require().NotEmpty(state.ChildRunID)
	s.Require().Equal(connector.CorrelationEnvelopeVersion, state.First.Version)
	s.Require().True(strings.HasPrefix(state.First.IdempotencyKey, publication.RunID+"/"))
	starts := 0
	for _, event := range s.fixtureEvidence() {
		if event["kind"] == "http_start" && event["workflow_id"] == publication.WorkflowID {
			starts++
			hint := event["hint"].(map[string]any)
			s.Require().Equal(state.First.RunID, hint["run_id"])
			s.Require().Equal(state.First.IdempotencyKey, hint["idempotency_key"])
		}
	}
	s.Require().GreaterOrEqual(starts, 2, "real post-admission activity retry must activate")
	s.Require().Len(s.fetchRuns(jobs[0]), 1, "at least two real HTTP starts must admit one run")
	histories := s.fixtureHistory(c, publication)
	childFound := false
	actorFound := false
	firstActivityID := ""
	for _, event := range histories {
		if scheduled := event.GetActivityTaskScheduledEventAttributes(); scheduled != nil && scheduled.GetActivityType().GetName() == "caesium.start" {
			firstActivityID = scheduled.GetActivityId()
		}
		if child := event.GetChildWorkflowExecutionStartedEventAttributes(); child != nil {
			childFound = child.GetWorkflowExecution().GetRunId() == state.ChildRunID
		}
	}
	s.Require().True(childFound, "child Run ID comes from actual Temporal history")
	s.Require().NotEmpty(firstActivityID)
	s.Require().Equal(publication.RunID+"/"+firstActivityID, state.First.IdempotencyKey, "key derives exactly from enclosing Temporal evidence")

	s.fixtureRequest(http.MethodPost, "/worker", map[string]bool{"running": false}, nil)
	s.Require().Equal(http.StatusServiceUnavailable, s.fixtureRequest(http.MethodGet, "/health", nil, nil), "a server without a worker is not ready")
	_, err = s.fixtureQuery(c, publication)
	s.Require().Error(err, "Query must not fabricate success without worker")
	s.fixtureRequest(http.MethodPost, "/worker", map[string]bool{"running": true}, nil)
	state = s.fixtureAwait(c, publication, "awaiting_approval")
	s.Require().Equal(0, state.ApprovalEffects)

	schema := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"note":{"type":"string"}}}`)
	_, err = connector.AcceptActionInput(schema, map[string]any{"_caesium_actor": actor}, actor)
	s.Require().Error(err, "public caller cannot forge the reserved actor before RPC")
	badActor := actor
	badActor.Role = "viewer"
	rejection, err := s.fixtureUpdate(c, publication, "invalid-actor", badActor)
	s.Require().NoError(err)
	s.Require().NotNil(rejection.GetOutcome().GetFailure(), "remote validator must refuse unauthorized actor role")
	s.Require().Equal(0, s.fixtureAwait(c, publication, "awaiting_approval").ApprovalEffects)

	acceptedID := "lost-accepted"
	s.fixtureRequest(http.MethodPost, "/fault", map[string]any{"update_id": acceptedID, "mode": "accepted"}, nil)
	_, err = s.fixtureUpdate(c, publication, acceptedID, actor)
	s.T().Logf("dropped Update error type=%T, code=%s", err, serviceerror.ToStatus(err).Code())
	s.Require().Equal(codes.DeadlineExceeded, serviceerror.ToStatus(err).Code())
	state = s.fixtureAwait(c, publication, "published")
	lookup, err := s.fixtureLookup(c, publication, acceptedID)
	s.Require().NoError(err)
	s.Require().Equal(enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED, lookup.GetStage())
	s.Require().NotNil(lookup.GetOutcome().GetSuccess())
	s.fixtureRequest(http.MethodPost, "/fault", map[string]any{}, nil)
	s.Require().Equal(&actor, state.Actor)
	s.Require().Equal(1, state.ApprovalEffects)
	s.fixtureRequest(http.MethodPost, "/worker", map[string]bool{"running": false}, nil)
	s.fixtureRequest(http.MethodPost, "/worker", map[string]bool{"running": true}, nil)
	s.Require().Equal(1, s.fixtureAwait(c, publication, "published").ApprovalEffects, "approval guard must survive actual worker replay")
	otherKey := s.createAPIKeyCLI("--role", "operator", "--description", "second connector fixture operator")
	otherActor := s.fixtureActor(otherKey, "other-operation")
	duplicate, err := s.fixtureUpdate(c, publication, "different-update-id", otherActor)
	s.Require().NoError(err)
	s.Require().NotNil(duplicate.GetOutcome().GetSuccess())
	state = s.fixtureAwait(c, publication, "published")
	s.Require().Equal(1, state.ApprovalEffects)
	s.Require().Equal(&actor, state.Actor)
	s.Require().Len(s.fetchRuns(jobs[1]), 1, "new update ID must not start the second job again")
	s.Require().Equal(1, fixtureCount(s.fixtureEvidence(), "approval_effect", "run_id", publication.RunID), "independent sink counts one business effect")
	s.Require().Equal(1, fixtureEffectActivities(s.fixtureEvidence(), publication.RunID), "one distinct effect activity across new IDs and worker replay")
	for _, event := range s.fixtureHistory(c, publication) {
		if accepted := event.GetWorkflowExecutionUpdateAcceptedEventAttributes(); accepted != nil && accepted.GetAcceptedRequest().GetMeta().GetUpdateId() == acceptedID {
			var envelope struct {
				Actor connector.ActorEnvelope `json:"_caesium_actor"`
			}
			s.Require().NoError(converter.GetDefaultDataConverter().FromPayloads(accepted.GetAcceptedRequest().GetInput().GetArgs(), &envelope))
			s.Require().Equal(actor, envelope.Actor)
			actorFound = true
		}
	}
	s.Require().True(actorFound, "authenticated producer context must survive in real accepted Update history")

	// Before acceptance a rejected Update is absent from history. Changed
	// eligibility and lookups must not silently submit the same unknown operation.
	rejected := s.fixtureStart("lost-rejected", "publication", jobs[0], jobs[1], false, false, false, false)
	s.fixtureAwait(c, rejected, "awaiting_approval")
	rejectedID := "lost-rejected"
	s.fixtureRequest(http.MethodPost, "/fault", map[string]any{"update_id": rejectedID, "mode": "rejected"}, nil)
	_, err = s.fixtureUpdate(c, rejected, rejectedID, actor)
	s.T().Logf("dropped Update error type=%T, code=%s", err, serviceerror.ToStatus(err).Code())
	s.Require().Equal(codes.DeadlineExceeded, serviceerror.ToStatus(err).Code())
	_, err = s.fixtureLookup(c, rejected, rejectedID)
	s.Require().Equal(codes.NotFound, serviceerror.ToStatus(err).Code(), "pinned server rejected-before-acceptance lookup is NotFound")
	callsBefore := fixtureCount(s.fixtureEvidence(), "update_rpc", "update_id", rejectedID)
	s.fixtureSignal(rejected, "fixture_eligibility", true)
	for i := 0; i < 2; i++ {
		_, err = s.fixtureLookup(c, rejected, rejectedID)
		s.Require().Equal(codes.NotFound, serviceerror.ToStatus(err).Code())
	}
	state = s.fixtureAwait(c, rejected, "awaiting_approval")
	s.Require().True(state.Eligible)
	s.Require().False(state.Approved)
	s.Require().Equal(0, fixtureCount(s.fixtureEvidence(), "approval_effect", "run_id", rejected.RunID))
	s.Require().Equal(callsBefore, fixtureCount(s.fixtureEvidence(), "update_rpc", "update_id", rejectedID), "lookup never resubmits after eligibility changes")
	for _, event := range s.fixtureHistory(c, rejected) {
		if accepted := event.GetWorkflowExecutionUpdateAcceptedEventAttributes(); accepted != nil {
			s.Require().NotEqual(rejectedID, accepted.GetAcceptedRequest().GetMeta().GetUpdateId())
		}
	}
	s.fixtureRequest(http.MethodPost, "/fault", map[string]any{}, nil)
	// This deliberate call models the original principal's explicit confirmation.
	resumed, err := s.fixtureUpdate(c, rejected, rejectedID, actor)
	s.Require().NoError(err)
	s.Require().NotNil(resumed.GetOutcome().GetSuccess())
	s.fixtureAwait(c, rejected, "published")
	s.Require().Equal(1, fixtureCount(s.fixtureEvidence(), "approval_effect", "run_id", rejected.RunID))

	opaque := s.fixtureStart("metadata", "publication", jobs[0], jobs[1], true, false, true, false)
	state = s.fixtureAwait(c, opaque, "awaiting_approval")
	s.Require().Nil(state.First, "opaque hints must not be exposed as decoded JSON")
	opaqueFound := false
	opaqueActivityID := ""
	for _, event := range s.fixtureHistory(c, opaque) {
		if scheduled := event.GetActivityTaskScheduledEventAttributes(); scheduled != nil && scheduled.GetActivityType().GetName() == "caesium.start" {
			opaqueActivityID = scheduled.GetActivityId()
		}
		if result := event.GetActivityTaskCompletedEventAttributes(); result != nil {
			for _, p := range result.GetResult().GetPayloads() {
				if string(p.GetMetadata()["encoding"]) == "binary/fixture" {
					opaqueFound = true
				}
			}
		}
	}
	s.Require().True(opaqueFound, "metadata-only activity must use a real unsupported result encoding")
	s.Require().NotEmpty(opaqueActivityID)
	history, err := observer.History(s.T().Context(), opaque.WorkflowID, opaque.RunID, nil, 100)
	s.Require().NoError(err)
	s.Require().NotEmpty(history.Activities)
	encoded, err := json.Marshal(history)
	s.Require().NoError(err)
	s.Require().NotContains(string(encoded), "opaque-correlation-canary")
	opaqueStarts := 0
	for _, event := range s.fixtureEvidence() {
		if event["kind"] == "http_start" && event["workflow_id"] == opaque.WorkflowID {
			opaqueStarts++
			hint := event["hint"].(map[string]any)
			s.Require().Equal(opaque.RunID+"/"+opaqueActivityID, hint["idempotency_key"])
			s.Require().NotEmpty(hint["run_id"])
		}
	}

	s.Require().Greater(opaqueStarts, 0, "metadata-only workflow must independently record a real HTTP admission")
	s.fixtureSignal(publication, "fixture_continue", true)
	var continuation string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		old, describeErr := c.WorkflowService().DescribeWorkflowExecution(s.T().Context(), &workflowservice.DescribeWorkflowExecutionRequest{Namespace: "default", Execution: &commonpb.WorkflowExecution{WorkflowId: publication.WorkflowID, RunId: publication.RunID}})
		if describeErr == nil && old.GetWorkflowExecutionInfo().GetStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW {
			for _, event := range s.fixtureHistory(c, publication) {
				if continued := event.GetWorkflowExecutionContinuedAsNewEventAttributes(); continued != nil {
					continuation = continued.GetNewExecutionRunId()
				}
			}
			if continuation != "" {
				break
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	s.Require().NotEmpty(continuation)
	s.Require().NotEqual(publication.RunID, continuation)
	newState := s.fixtureAwait(c, fixtureRef{WorkflowID: publication.WorkflowID, RunID: continuation}, "published")
	s.Require().Equal(1, newState.ApprovalEffects)
	oldAction, oldErr := s.fixtureUpdate(c, publication, "old-run-action", otherActor)
	s.Require().True(oldErr != nil || oldAction.GetOutcome().GetFailure() != nil, "closed old run must not retarget continuation")
	s.Require().Equal(0, fixtureCount(s.fixtureEvidence(), "approval_effect", "run_id", continuation))

	s.fixtureRequest(http.MethodPost, "/fault", map[string]any{"outage": true}, nil)
	_, err = observer.Describe(s.T().Context(), publication.WorkflowID, continuation)
	s.Require().Error(err)
	s.fixtureRequest(http.MethodPost, "/fault", map[string]any{}, nil)
	recovered, err := observer.Describe(s.T().Context(), publication.WorkflowID, continuation)
	s.Require().NoError(err)
	s.Require().Equal(continuation, recovered.RunID)
	events := s.fixtureEvidence()
	acceptedDrop, rejectedDrop, interruption := false, false, false
	for _, event := range events {
		if event["kind"] == "update_rpc" && event["reply_dropped"] == true {
			if event["update_id"] == acceptedID {
				acceptedDrop = true
				s.Require().Equal(false, event["rejected"])
			}
			if event["update_id"] == rejectedID {
				rejectedDrop = true
				s.Require().Equal(true, event["rejected"])
			}
		}
		if event["kind"] == "proxy_interruption" {
			interruption = true
		}
	}
	s.Require().True(acceptedDrop, "accepted reply-loss fault must actually activate")
	s.Require().True(rejectedDrop, "rejected reply-loss fault must actually activate")
	s.Require().True(interruption)
	data, err := json.MarshalIndent(map[string]any{"candidate_sha": candidate, "sdk": "1.49.0", "cli": "1.9.1", "server": system.GetServerVersion(), "accepted_lookup": "COMPLETED/OK", "rejected_lookup": "NOT_FOUND", "publication": publication, "continuation_run_id": continuation, "events": events}, "", "  ")
	s.Require().NoError(err)
	s.Require().NoError(os.WriteFile(filepath.Join(artifacts, "fixture-evidence.json"), data, 0644))
}
