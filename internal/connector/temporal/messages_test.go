package temporal

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/connector"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	failurepb "go.temporal.io/api/failure/v1"
	querypb "go.temporal.io/api/query/v1"
	"go.temporal.io/api/serviceerror"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	updatepb "go.temporal.io/api/update/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeMessageService struct {
	describe func(context.Context, *workflowservice.DescribeWorkflowExecutionRequest) (*workflowservice.DescribeWorkflowExecutionResponse, error)
	query    func(context.Context, *workflowservice.QueryWorkflowRequest) (*workflowservice.QueryWorkflowResponse, error)
	submit   func(context.Context, *workflowservice.UpdateWorkflowExecutionRequest) (*workflowservice.UpdateWorkflowExecutionResponse, error)
	poll     func(context.Context, *workflowservice.PollWorkflowExecutionUpdateRequest) (*workflowservice.PollWorkflowExecutionUpdateResponse, error)
	worker   func(context.Context, *workflowservice.DescribeTaskQueueRequest) (*workflowservice.DescribeTaskQueueResponse, error)
	sends    int
	polls    int
	queries  int
	reads    int
	workers  int
}

func (f *fakeMessageService) DescribeWorkflowExecution(ctx context.Context, request *workflowservice.DescribeWorkflowExecutionRequest, _ ...grpc.CallOption) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	f.reads++
	if f.describe != nil {
		return f.describe(ctx, request)
	}
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Execution: request.Execution, Type: &commonpb.WorkflowType{Name: "publication"}},
		ExecutionConfig:       &workflowpb.WorkflowExecutionConfig{TaskQueue: &taskqueuepb.TaskQueue{Name: "publication-queue"}},
	}, nil
}

func (f *fakeMessageService) QueryWorkflow(ctx context.Context, request *workflowservice.QueryWorkflowRequest, _ ...grpc.CallOption) (*workflowservice.QueryWorkflowResponse, error) {
	f.queries++
	return f.query(ctx, request)
}

func (f *fakeMessageService) UpdateWorkflowExecution(ctx context.Context, request *workflowservice.UpdateWorkflowExecutionRequest, _ ...grpc.CallOption) (*workflowservice.UpdateWorkflowExecutionResponse, error) {
	f.sends++
	return f.submit(ctx, request)
}

func (f *fakeMessageService) PollWorkflowExecutionUpdate(ctx context.Context, request *workflowservice.PollWorkflowExecutionUpdateRequest, _ ...grpc.CallOption) (*workflowservice.PollWorkflowExecutionUpdateResponse, error) {
	f.polls++
	return f.poll(ctx, request)
}

func (f *fakeMessageService) DescribeTaskQueue(ctx context.Context, request *workflowservice.DescribeTaskQueueRequest, _ ...grpc.CallOption) (*workflowservice.DescribeTaskQueueResponse, error) {
	f.workers++
	return f.worker(ctx, request)
}

const queryMessageSchema = `{"type":"object","additionalProperties":false,"required":["stage"],"properties":{"stage":{"type":"string"}}}`
const inputMessageSchema = `{"type":"object","additionalProperties":false,"properties":{"note":{"type":"string"}}}`
const resultMessageSchema = `{"type":"object","additionalProperties":false,"required":["approved"],"properties":{"approved":{"type":"boolean"}}}`

func messageBindings() []connector.Binding {
	return []connector.Binding{{Name: "publication", Version: "1", StatusQuery: "publication_status", StatusQueryResultSchema: json.RawMessage(queryMessageSchema), Actions: []connector.Action{{Name: "approve_publication", InputSchema: json.RawMessage(inputMessageSchema), ResultSchema: json.RawMessage(resultMessageSchema)}}}}
}

func pinnedMessageTarget() MessageTarget {
	return MessageTarget{ConnectionID: "primary", WorkflowID: "publication-workflow", RunID: "original-run", BindingName: "publication", BindingVersion: "1"}
}

func messageActor() connector.ActorEnvelope {
	return connector.ActorEnvelope{PrincipalKind: connector.PrincipalKindAPIKey, StableID: "key-id", Subject: "key-owner", Role: "operator", OperationID: "persisted-operation", BindingVersion: "1"}
}

func mustMessenger(t *testing.T, service *fakeMessageService) *Messenger {
	t.Helper()
	m, err := NewMessenger(service, "primary", "namespace", messageBindings(), "secret-token-never-output", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func mustUpdateID(t *testing.T) string {
	t.Helper()
	id, err := StableUpdateID(messageActor().OperationID)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func updateRef(request *workflowservice.UpdateWorkflowExecutionRequest) *updatepb.UpdateRef {
	return &updatepb.UpdateRef{WorkflowExecution: request.WorkflowExecution, UpdateId: request.Request.Meta.UpdateId}
}

func completedMessageOutcome(raw string) *updatepb.Outcome {
	return &updatepb.Outcome{Value: &updatepb.Outcome_Success{Success: jsonPayloads([]byte(raw))}}
}

func requireLocalNotDispatched(t *testing.T, result UpdateResult, err error) {
	t.Helper()
	if err == nil || result.State != UpdateNotDispatched || result.RPCCode != "" || result.PreflightRPCCode != "" {
		t.Fatalf("local refusal was not distinguishable from dispatch/describe failure: %+v %v", result, err)
	}
}

func TestDeclaredStatusQueryPinsTargetAndValidatesSchema(t *testing.T) {
	service := &fakeMessageService{query: func(ctx context.Context, request *workflowservice.QueryWorkflowRequest) (*workflowservice.QueryWorkflowResponse, error) {
		if request.Namespace != "namespace" || request.Execution.RunId != "original-run" || request.Execution.WorkflowId != "publication-workflow" || request.Query.QueryType != "publication_status" || request.Query.QueryArgs != nil {
			t.Fatalf("Query target/handler changed: %+v", request)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > connector.MaxRPCDeadline {
			t.Fatal("Query lacks its bounded deadline")
		}
		return &workflowservice.QueryWorkflowResponse{QueryResult: jsonPayloads([]byte(`{"stage":"awaiting_approval"}`))}, nil
	}}
	result, err := mustMessenger(t, service).QueryStatus(context.Background(), pinnedMessageTarget())
	if err != nil || string(result) != `{"stage":"awaiting_approval"}` || service.queries != 1 || service.reads != 1 || service.sends != 0 || service.workers != 0 {
		t.Fatalf("Query result=%s err=%v calls=%+v", result, err, service)
	}
}

func TestMessageTargetsAndUndeclaredHandlersRefuseBeforeDispatch(t *testing.T) {
	for _, mutation := range []func(*MessageTarget){
		func(target *MessageTarget) { target.RunID = "" },
		func(target *MessageTarget) { target.WorkflowID = "" },
		func(target *MessageTarget) { target.ConnectionID = "other" },
		func(target *MessageTarget) { target.BindingName = "unbound" },
		func(target *MessageTarget) { target.BindingVersion = "changed" },
	} {
		service := &fakeMessageService{}
		m := mustMessenger(t, service)
		target := pinnedMessageTarget()
		mutation(&target)
		if _, err := m.QueryStatus(t.Context(), target); err == nil {
			t.Fatal("invalid Query target accepted")
		}
		result, err := m.SubmitAction(t.Context(), target, "approve_publication", mustUpdateID(t), nil, messageActor())
		requireLocalNotDispatched(t, result, err)
		result, err = m.LookupAction(t.Context(), target, "approve_publication", mustUpdateID(t))
		requireLocalNotDispatched(t, result, err)
		if service.reads+service.queries+service.sends+service.polls != 0 {
			t.Fatal("invalid target reached Temporal")
		}
	}
	service := &fakeMessageService{}
	m := mustMessenger(t, service)
	for _, name := range []string{"arbitrary_signal", "terminate", "retry"} {
		result, err := m.SubmitAction(t.Context(), pinnedMessageTarget(), name, mustUpdateID(t), nil, messageActor())
		requireLocalNotDispatched(t, result, err)
		result, err = m.LookupAction(t.Context(), pinnedMessageTarget(), name, mustUpdateID(t))
		requireLocalNotDispatched(t, result, err)
	}
	result, err := m.LookupAction(t.Context(), pinnedMessageTarget(), "approve_publication", "")
	requireLocalNotDispatched(t, result, err)
	result, err = m.SubmitAction(t.Context(), pinnedMessageTarget(), "approve_publication", "", nil, messageActor())
	requireLocalNotDispatched(t, result, err)
	if service.reads != 0 {
		t.Fatal("blank stable Update id reached provider")
	}
}

func TestActualWorkflowTypeAndRunMustMatchBinding(t *testing.T) {
	for _, mismatch := range []string{"type", "run", "workflow", "missing"} {
		t.Run(mismatch, func(t *testing.T) {
			service := &fakeMessageService{describe: func(_ context.Context, request *workflowservice.DescribeWorkflowExecutionRequest) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
				info := &workflowpb.WorkflowExecutionInfo{Execution: workflowExecution(pinnedMessageTarget()), Type: &commonpb.WorkflowType{Name: "publication"}}
				switch mismatch {
				case "type":
					info.Type.Name = "unbound"
				case "run":
					info.Execution.RunId = "newer-run"
				case "workflow":
					info.Execution.WorkflowId = "other-workflow"
				case "missing":
					return nil, nil
				}
				return &workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: info}, nil
			}}
			m := mustMessenger(t, service)
			if _, err := m.QueryStatus(t.Context(), pinnedMessageTarget()); err == nil {
				t.Fatal("mismatched Query execution accepted")
			}
			result, err := m.SubmitAction(t.Context(), pinnedMessageTarget(), "approve_publication", mustUpdateID(t), nil, messageActor())
			requireLocalNotDispatched(t, result, err)
			if !strings.Contains(err.Error(), "does not match the pinned binding") {
				t.Fatalf("identity refusal lost its descriptive error: %v", err)
			}
			result, err = m.LookupAction(t.Context(), pinnedMessageTarget(), "approve_publication", mustUpdateID(t))
			requireLocalNotDispatched(t, result, err)
			if service.queries+service.sends+service.polls != 0 {
				t.Fatal("mismatched execution dispatched a message")
			}
		})
	}
}

func TestUpdateEnvelopeSeparatesValidatedInputAndAuthenticatedActor(t *testing.T) {
	service := &fakeMessageService{submit: func(_ context.Context, request *workflowservice.UpdateWorkflowExecutionRequest) (*workflowservice.UpdateWorkflowExecutionResponse, error) {
		if request.Namespace != "namespace" || request.WorkflowExecution.RunId != "original-run" || request.Request.Input.Name != "approve_publication" || request.Request.Meta.UpdateId != mustUpdateID(t) || request.WaitPolicy.LifecycleStage != enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_ACCEPTED {
			t.Fatalf("Update did not preserve pinned handler/identity: %+v", request)
		}
		var envelope struct {
			Input map[string]any          `json:"input"`
			Actor connector.ActorEnvelope `json:"_caesium_actor"`
		}
		payload := request.Request.Input.Args.Payloads[0]
		if string(payload.Metadata["encoding"]) != "json/plain" || json.Unmarshal(payload.Data, &envelope) != nil || envelope.Input["note"] != "publish" || envelope.Actor != messageActor() {
			t.Fatalf("workflow-visible actor/input mismatch: %s", payload.Data)
		}
		return &workflowservice.UpdateWorkflowExecutionResponse{UpdateRef: updateRef(request), Stage: enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_ACCEPTED}, nil
	}}
	result, err := mustMessenger(t, service).SubmitAction(t.Context(), pinnedMessageTarget(), "approve_publication", mustUpdateID(t), map[string]any{"note": "publish"}, messageActor())
	if err != nil || result.State != UpdateAccepted || service.sends != 1 || service.polls != 0 {
		t.Fatalf("acceptance result=%+v err=%v sends=%d polls=%d", result, err, service.sends, service.polls)
	}
}

func TestActionInputActorSpoofAndIDMismatchRefuseBeforeRPC(t *testing.T) {
	for _, input := range []map[string]any{
		{"note": 12},
		{"arbitrary": "field"},
		{connector.ReservedActorField: map[string]any{"subject": "forged"}},
		{"note": make(chan int)},
	} {
		service := &fakeMessageService{}
		result, err := mustMessenger(t, service).SubmitAction(t.Context(), pinnedMessageTarget(), "approve_publication", mustUpdateID(t), input, messageActor())
		requireLocalNotDispatched(t, result, err)
		if service.reads != 0 {
			t.Fatal("invalid/spoofed input dispatched")
		}
	}
	for _, mutation := range []func(*connector.ActorEnvelope){
		func(actor *connector.ActorEnvelope) { actor.Role = "viewer" },
		func(actor *connector.ActorEnvelope) { actor.StableID = "" },
		func(actor *connector.ActorEnvelope) { actor.BindingVersion = "new" },
		func(actor *connector.ActorEnvelope) { actor.OperationID = "another-receipt" },
	} {
		service := &fakeMessageService{}
		actor := messageActor()
		mutation(&actor)
		result, err := mustMessenger(t, service).SubmitAction(t.Context(), pinnedMessageTarget(), "approve_publication", mustUpdateID(t), nil, actor)
		requireLocalNotDispatched(t, result, err)
		if service.reads != 0 {
			t.Fatal("invalid actor/operation dispatched")
		}
	}
}

func TestDescribeRPCFailureIsNotAnAmbiguousDispatch(t *testing.T) {
	for _, rpcErr := range []error{
		serviceerror.NewUnavailable("secret-describe-message"),
		serviceerror.NewNotFound("secret-describe-message"),
		context.DeadlineExceeded,
		context.Canceled,
		status.Error(codes.Unknown, "secret-describe-message"),
	} {
		service := &fakeMessageService{describe: func(context.Context, *workflowservice.DescribeWorkflowExecutionRequest) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
			return nil, rpcErr
		}}
		m := mustMessenger(t, service)
		for _, invoke := range []func() (UpdateResult, error){
			func() (UpdateResult, error) {
				return m.SubmitAction(t.Context(), pinnedMessageTarget(), "approve_publication", mustUpdateID(t), nil, messageActor())
			},
			func() (UpdateResult, error) {
				return m.LookupAction(t.Context(), pinnedMessageTarget(), "approve_publication", mustUpdateID(t))
			},
		} {
			result, err := invoke()
			if err == nil || result.State != UpdateNotDispatched || result.PreflightRPCCode != rpcCode(rpcErr).String() || result.RPCCode != "" || strings.Contains(err.Error(), "secret") {
				t.Fatalf("describe fault looked like an uncertain Update/Poll dispatch: %+v %v", result, err)
			}
		}
		if service.reads != 2 || service.sends+service.polls != 0 {
			t.Fatalf("failed preflight dispatched: describe=%d Update=%d Poll=%d", service.reads, service.sends, service.polls)
		}
	}
}

func TestCanceledBeforeDispatchHasNoFabricatedRPCCode(t *testing.T) {
	for _, duringDescribe := range []bool{false, true} {
		for _, lookup := range []bool{false, true} {
			ctx, cancel := context.WithCancel(t.Context())
			service := &fakeMessageService{}
			if duringDescribe {
				service.describe = func(_ context.Context, request *workflowservice.DescribeWorkflowExecutionRequest) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
					cancel()
					return &workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Execution: request.Execution, Type: &commonpb.WorkflowType{Name: "publication"}}}, nil
				}
			} else {
				cancel()
			}
			m := mustMessenger(t, service)
			var result UpdateResult
			var err error
			if lookup {
				result, err = m.LookupAction(ctx, pinnedMessageTarget(), "approve_publication", mustUpdateID(t))
			} else {
				result, err = m.SubmitAction(ctx, pinnedMessageTarget(), "approve_publication", mustUpdateID(t), nil, messageActor())
			}
			cancel()
			requireLocalNotDispatched(t, result, err)
			if !errors.Is(err, context.Canceled) || service.sends+service.polls != 0 {
				t.Fatalf("canceled call attempted dispatch or lost cancellation: %+v %v", result, err)
			}
			wantReads := 0
			if duringDescribe {
				wantReads = 1
			}
			if service.reads != wantReads {
				t.Fatalf("canceled preflight read count=%d want=%d", service.reads, wantReads)
			}
		}
	}
}

func TestSubmissionFailureCannotProveValidatorRejection(t *testing.T) {
	// Both validator rejection and an accepted handler failure share this
	// completed failure shape. Remote failure text cannot prove its origin.
	for _, message := range []string{"publication is not eligible", "handler failed after effects", "Workflow Update failed because the Workflow completed before the Update completed"} {
		service := &fakeMessageService{submit: func(_ context.Context, request *workflowservice.UpdateWorkflowExecutionRequest) (*workflowservice.UpdateWorkflowExecutionResponse, error) {
			return &workflowservice.UpdateWorkflowExecutionResponse{UpdateRef: updateRef(request), Stage: enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED, Outcome: &updatepb.Outcome{Value: &updatepb.Outcome_Failure{Failure: &failurepb.Failure{Message: message}}}}, nil
		}}
		result, err := mustMessenger(t, service).SubmitAction(t.Context(), pinnedMessageTarget(), "approve_publication", mustUpdateID(t), nil, messageActor())
		if err != nil || result.State != UpdateUnknown || result.FailureKind != "failure_origin_unknown" || result.RPCCode != "" || result.PreflightRPCCode != "" || service.sends != 1 {
			t.Fatalf("ambiguous failure was classified as no-effect rejection: %+v %v", result, err)
		}
		encoded, _ := json.Marshal(result)
		if strings.Contains(string(encoded), message) {
			t.Fatalf("remote failure details leaked: %s", encoded)
		}
	}
}

func TestLostRPCRepliesAndNotFoundStayUnknownWithoutResubmit(t *testing.T) {
	for _, rpcErr := range []error{
		status.Error(codes.Unavailable, "token-secret-remote-message"),
		serviceerror.NewNotFound("rejection disappeared"),
		serviceerror.NewInvalidArgument("not a validator outcome"),
		context.DeadlineExceeded,
		context.Canceled,
	} {
		service := &fakeMessageService{
			submit: func(context.Context, *workflowservice.UpdateWorkflowExecutionRequest) (*workflowservice.UpdateWorkflowExecutionResponse, error) {
				return nil, rpcErr
			},
			poll: func(_ context.Context, request *workflowservice.PollWorkflowExecutionUpdateRequest) (*workflowservice.PollWorkflowExecutionUpdateResponse, error) {
				if request.UpdateRef.WorkflowExecution.RunId != "original-run" || request.UpdateRef.UpdateId != mustUpdateID(t) || request.WaitPolicy != nil {
					t.Fatal("lookup did not preserve exact non-blocking Update reference")
				}
				return nil, rpcErr
			},
		}
		m := mustMessenger(t, service)
		submitted, err := m.SubmitAction(t.Context(), pinnedMessageTarget(), "approve_publication", mustUpdateID(t), nil, messageActor())
		if err == nil || submitted.State != UpdateUnknown || submitted.RPCCode != rpcCode(rpcErr).String() || submitted.PreflightRPCCode != "" || strings.Contains(err.Error(), "token-secret") {
			t.Fatalf("ambiguous submission became terminal or leaked text: %+v %v", submitted, err)
		}
		for range 3 {
			lookedUp, err := m.LookupAction(t.Context(), pinnedMessageTarget(), "approve_publication", mustUpdateID(t))
			if err == nil || lookedUp.State != UpdateUnknown || lookedUp.RPCCode != rpcCode(rpcErr).String() || lookedUp.PreflightRPCCode != "" {
				t.Fatalf("RPC lookup error became known rejection: %+v %v", lookedUp, err)
			}
		}
		if service.sends != 1 || service.polls != 3 || service.workers != 0 {
			t.Fatalf("lookup implicitly resent or diagnosed worker: sends=%d polls=%d", service.sends, service.polls)
		}
	}
}

func TestLookupOutcomeDistinguishesReturnedFailureAndAcceptance(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		stage   enumspb.UpdateWorkflowExecutionLifecycleStage
		outcome *updatepb.Outcome
		state   string
		wantErr bool
	}{
		{"completed", enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED, completedMessageOutcome(`{"approved":true}`), UpdateCompleted, false},
		{"handler failure", enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED, &updatepb.Outcome{Value: &updatepb.Outcome_Failure{Failure: &failurepb.Failure{Message: "secret-handler-message"}}}, UpdateUnknown, false},
		{"in-flight validator rejection", enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED, &updatepb.Outcome{Value: &updatepb.Outcome_Failure{Failure: &failurepb.Failure{Message: "secret-validator-message"}}}, UpdateUnknown, false},
		{"admitted", enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_ADMITTED, nil, UpdateSubmitted, false},
		{"accepted", enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_ACCEPTED, nil, UpdateAccepted, false},
		{"unspecified", enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_UNSPECIFIED, nil, UpdateUnknown, false},
		{"missing outcome", enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED, nil, UpdateUnknown, true},
		{"bad result", enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED, completedMessageOutcome(`{"approved":"wrong"}`), UpdateUnknown, true},
		{"undeclared field", enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED, completedMessageOutcome(`{"approved":true,"private":"secret"}`), UpdateUnknown, true},
		{"premature outcome", enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_ACCEPTED, completedMessageOutcome(`{"approved":true}`), UpdateUnknown, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			service := &fakeMessageService{poll: func(_ context.Context, request *workflowservice.PollWorkflowExecutionUpdateRequest) (*workflowservice.PollWorkflowExecutionUpdateResponse, error) {
				return &workflowservice.PollWorkflowExecutionUpdateResponse{UpdateRef: request.UpdateRef, Stage: scenario.stage, Outcome: scenario.outcome}, nil
			}}
			result, err := mustMessenger(t, service).LookupAction(t.Context(), pinnedMessageTarget(), "approve_publication", mustUpdateID(t))
			if (err != nil) != scenario.wantErr || result.State != scenario.state || service.sends != 0 {
				t.Fatalf("outcome=%+v err=%v", result, err)
			}
			if scenario.outcome.GetFailure() != nil && result.FailureKind != "failure_origin_unknown" {
				t.Fatalf("lookup failure claimed an unproven origin: %+v", result)
			}
			encoded, _ := json.Marshal(result)
			if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "private") {
				t.Fatalf("unvalidated result/failure payload leaked: %s", encoded)
			}
		})
	}
}

func TestDefaultJSONOnlyAndSchemaRequiredForStatusQuery(t *testing.T) {
	for _, raw := range []string{`{"stage":12}`, `{"private":"secret"}`, `null`, `[]`, `{"stage":"ok"} {"private":"secret"}`} {
		service := &fakeMessageService{query: func(context.Context, *workflowservice.QueryWorkflowRequest) (*workflowservice.QueryWorkflowResponse, error) {
			return &workflowservice.QueryWorkflowResponse{QueryResult: jsonPayloads([]byte(raw))}, nil
		}}
		if result, err := mustMessenger(t, service).QueryStatus(t.Context(), pinnedMessageTarget()); err == nil || len(result) != 0 {
			t.Fatalf("invalid Query result exposed: %s %v", result, err)
		}
	}
	for _, encoding := range []string{"binary/encrypted", "binary/protobuf", "json/protobuf", ""} {
		service := &fakeMessageService{query: func(context.Context, *workflowservice.QueryWorkflowRequest) (*workflowservice.QueryWorkflowResponse, error) {
			payload := jsonPayloads([]byte(`{"stage":"ok"}`))
			payload.Payloads[0].Metadata["encoding"] = []byte(encoding)
			return &workflowservice.QueryWorkflowResponse{QueryResult: payload}, nil
		}}
		if result, err := mustMessenger(t, service).QueryStatus(t.Context(), pinnedMessageTarget()); err == nil || len(result) != 0 {
			t.Fatalf("custom conversion exposed: %s %v", result, err)
		}
	}
	service := &fakeMessageService{}
	bindings := messageBindings()
	bindings[0].StatusQueryResultSchema = nil
	m, err := NewMessenger(service, "primary", "namespace", bindings, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.QueryStatus(t.Context(), pinnedMessageTarget()); err == nil || service.reads != 0 {
		t.Fatal("legacy binding without Query schema dispatched")
	}
}

func TestMessengerFreezesBindingsAndRespectsBudgetAndShortDeadline(t *testing.T) {
	service := &fakeMessageService{query: func(ctx context.Context, request *workflowservice.QueryWorkflowRequest) (*workflowservice.QueryWorkflowResponse, error) {
		if request.Query.QueryType != "publication_status" {
			t.Fatal("caller mutated the frozen Query")
		}
		deadline, _ := ctx.Deadline()
		if time.Until(deadline) > time.Second {
			t.Fatal("short caller deadline was extended")
		}
		return &workflowservice.QueryWorkflowResponse{QueryResult: jsonPayloads([]byte(`{"stage":"` + strings.Repeat("large", 200) + `"}`))}, nil
	}}
	bindings := messageBindings()
	m, err := NewMessenger(service, "primary", "namespace", bindings, "", 0, 128)
	if err != nil {
		t.Fatal(err)
	}
	bindings[0].StatusQuery = "private_query"
	bindings[0].StatusQueryResultSchema[0] = '['
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if result, err := m.QueryStatus(ctx, pinnedMessageTarget()); err == nil || len(result) != 0 {
		t.Fatal("over-budget Query result exposed")
	}
	result, err := m.SubmitAction(t.Context(), pinnedMessageTarget(), "approve_publication", mustUpdateID(t), map[string]any{"note": strings.Repeat("a", 129)}, messageActor())
	requireLocalNotDispatched(t, result, err)
	if service.sends != 0 {
		t.Fatal("over-budget input dispatched")
	}
	result, err = m.SubmitAction(t.Context(), pinnedMessageTarget(), "approve_publication", mustUpdateID(t), nil, messageActor())
	requireLocalNotDispatched(t, result, err)
	if service.sends != 0 {
		t.Fatal("actor envelope budget was not included")
	}
	for _, bad := range []struct {
		deadline time.Duration
		bytes    int
	}{
		{-time.Second, 0}, {connector.MaxRPCDeadline + time.Second, 0}, {0, -1}, {0, connector.MaxPageMetadataBytes + 1},
	} {
		if _, err := NewMessenger(service, "primary", "namespace", messageBindings(), "", bad.deadline, bad.bytes); err == nil {
			t.Fatal("invalid message limit accepted")
		}
	}
}

func TestResponseUpdateReferenceCannotRetarget(t *testing.T) {
	for _, mismatch := range []string{"run", "workflow", "update", "missing reference", "missing response"} {
		t.Run(mismatch, func(t *testing.T) {
			service := &fakeMessageService{poll: func(_ context.Context, request *workflowservice.PollWorkflowExecutionUpdateRequest) (*workflowservice.PollWorkflowExecutionUpdateResponse, error) {
				ref := &updatepb.UpdateRef{WorkflowExecution: &commonpb.WorkflowExecution{WorkflowId: "publication-workflow", RunId: "original-run"}, UpdateId: request.UpdateRef.UpdateId}
				switch mismatch {
				case "run":
					ref.WorkflowExecution.RunId = "continued-run"
				case "workflow":
					ref.WorkflowExecution.WorkflowId = "other-workflow"
				case "update":
					ref.UpdateId = "other-update"
				case "missing reference":
					ref = nil
				case "missing response":
					return nil, nil
				}
				return &workflowservice.PollWorkflowExecutionUpdateResponse{UpdateRef: ref, Stage: enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED, Outcome: completedMessageOutcome(`{"approved":true}`)}, nil
			}}
			result, err := mustMessenger(t, service).LookupAction(t.Context(), pinnedMessageTarget(), "approve_publication", mustUpdateID(t))
			if err == nil || result.State != UpdateUnknown || result.PreflightRPCCode != "" || result.RPCCode != "" || len(result.Result) != 0 || service.sends != 0 || service.polls != 1 {
				t.Fatalf("mismatched dispatched lookup looked like a refusal, RPC failure, or retargeted result: %+v %v", result, err)
			}
		})
	}
}

func TestWorkerEvidenceIsExplicitAndNotAnUpdateOutcome(t *testing.T) {
	for _, recent := range []bool{false, true} {
		service := &fakeMessageService{worker: func(_ context.Context, request *workflowservice.DescribeTaskQueueRequest) (*workflowservice.DescribeTaskQueueResponse, error) {
			if request.TaskQueue.Name != "publication-queue" || request.TaskQueueType != enumspb.TASK_QUEUE_TYPE_WORKFLOW {
				t.Fatal("worker diagnostic looked up another task queue")
			}
			response := &workflowservice.DescribeTaskQueueResponse{}
			if recent {
				response.Pollers = []*taskqueuepb.PollerInfo{{Identity: "private-worker-identity"}}
			}
			return response, nil
		}}
		observation, err := mustMessenger(t, service).CheckWorker(t.Context(), pinnedMessageTarget())
		want := "no_recent_pollers"
		if recent {
			want = "recent_pollers"
		}
		if err != nil || observation.Availability != want || service.sends+service.queries+service.polls != 0 {
			t.Fatalf("worker evidence altered message state: %+v %v", observation, err)
		}
	}
	service := &fakeMessageService{worker: func(context.Context, *workflowservice.DescribeTaskQueueRequest) (*workflowservice.DescribeTaskQueueResponse, error) {
		return nil, status.Error(codes.Unavailable, "secret-unavailable-details")
	}}
	result, err := mustMessenger(t, service).CheckWorker(t.Context(), pinnedMessageTarget())
	if result.Availability != "unknown" || result.RPCCode != "Unavailable" || err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("worker outage misclassified or leaked: %+v %v", result, err)
	}
}

func TestQueryRejectionAndRPCFailureExposeNoPayload(t *testing.T) {
	for _, scenario := range []struct {
		response *workflowservice.QueryWorkflowResponse
		err      error
	}{
		{&workflowservice.QueryWorkflowResponse{QueryRejected: &querypb.QueryRejected{}}, nil},
		{nil, nil},
		{nil, errors.New("private-secret-error")},
	} {
		service := &fakeMessageService{query: func(context.Context, *workflowservice.QueryWorkflowRequest) (*workflowservice.QueryWorkflowResponse, error) {
			return scenario.response, scenario.err
		}}
		result, err := mustMessenger(t, service).QueryStatus(t.Context(), pinnedMessageTarget())
		if len(result) != 0 || err == nil || strings.Contains(err.Error(), "private-secret") {
			t.Fatalf("Query failure exposed result/error: %s %v", result, err)
		}
	}
}

func TestDeclaredJSONResultsRedactCredentialsAndPEM(t *testing.T) {
	for _, stage := range []string{"secret-token-never-output", "-----BEGIN PRIVATE KEY-----\nprivate bytes\n-----END PRIVATE KEY-----"} {
		service := &fakeMessageService{query: func(context.Context, *workflowservice.QueryWorkflowRequest) (*workflowservice.QueryWorkflowResponse, error) {
			encoded, _ := json.Marshal(map[string]any{"stage": stage})
			return &workflowservice.QueryWorkflowResponse{QueryResult: jsonPayloads(encoded)}, nil
		}}
		result, err := mustMessenger(t, service).QueryStatus(t.Context(), pinnedMessageTarget())
		if err != nil || string(result) != `{"stage":"[redacted]"}` {
			t.Fatalf("declared result secret was not redacted: %s %v", result, err)
		}
	}
}

func TestSensitiveNestedKeysWithholdQueryAndUpdateResults(t *testing.T) {
	for _, secret := range []struct{ key, apiKey string }{
		{"secret-token-never-output", "secret-token-never-output"},
		{"-----BEGIN PRIVATE KEY-----\nprivate bytes\n-----END PRIVATE KEY-----", "secret-token-never-output"},
		{"secret", "secret"},
		{"[redacted]", "[redacted]"},
	} {
		sensitive := secret.key
		bindings := messageBindings()
		bindings[0].StatusQueryResultSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"metadata":{"type":"object"}}}`)
		bindings[0].Actions[0].ResultSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"metadata":{"type":"object"}}}`)
		// A replacement key would collide with existing data. The whole result
		// must fail closed rather than exposing keys or discarding a value.
		encoded, _ := json.Marshal(map[string]any{"metadata": map[string]any{sensitive: "sensitive-key-value", "[redacted]": "existing-value", "ordinary": "remaining-value"}})
		service := &fakeMessageService{
			query: func(context.Context, *workflowservice.QueryWorkflowRequest) (*workflowservice.QueryWorkflowResponse, error) {
				return &workflowservice.QueryWorkflowResponse{QueryResult: jsonPayloads(encoded)}, nil
			},
			poll: func(_ context.Context, request *workflowservice.PollWorkflowExecutionUpdateRequest) (*workflowservice.PollWorkflowExecutionUpdateResponse, error) {
				return &workflowservice.PollWorkflowExecutionUpdateResponse{UpdateRef: request.UpdateRef, Stage: enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED, Outcome: completedMessageOutcome(string(encoded))}, nil
			},
		}
		m, err := NewMessenger(service, "primary", "namespace", bindings, secret.apiKey, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		query, err := m.QueryStatus(t.Context(), pinnedMessageTarget())
		if err == nil || len(query) != 0 || strings.Contains(err.Error(), sensitive) {
			t.Fatalf("sensitive nested Query key exposed: %s %v", query, err)
		}
		update, err := m.LookupAction(t.Context(), pinnedMessageTarget(), "approve_publication", mustUpdateID(t))
		if err == nil || update.State != UpdateUnknown || len(update.Result) != 0 || strings.Contains(err.Error(), sensitive) {
			t.Fatalf("sensitive nested Update key exposed: %+v %v", update, err)
		}
	}
}

func TestShortKnownCredentialsAreRedactedFromDeclaredValues(t *testing.T) {
	for _, credential := range []string{"secret", "red"} {
		bindings := messageBindings()
		bindings[0].Actions[0].ResultSchema = json.RawMessage(queryMessageSchema)
		encoded, _ := json.Marshal(map[string]any{"stage": credential})
		service := &fakeMessageService{
			query: func(context.Context, *workflowservice.QueryWorkflowRequest) (*workflowservice.QueryWorkflowResponse, error) {
				return &workflowservice.QueryWorkflowResponse{QueryResult: jsonPayloads(encoded)}, nil
			},
			poll: func(_ context.Context, request *workflowservice.PollWorkflowExecutionUpdateRequest) (*workflowservice.PollWorkflowExecutionUpdateResponse, error) {
				return &workflowservice.PollWorkflowExecutionUpdateResponse{UpdateRef: request.UpdateRef, Stage: enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED, Outcome: completedMessageOutcome(string(encoded))}, nil
			},
		}
		m, err := NewMessenger(service, "primary", "namespace", bindings, credential, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		query, err := m.QueryStatus(t.Context(), pinnedMessageTarget())
		if err != nil || len(query) == 0 || strings.Contains(string(query), credential) {
			t.Fatalf("short credential remained in declared Query value: %s %v", query, err)
		}
		update, err := m.LookupAction(t.Context(), pinnedMessageTarget(), "approve_publication", mustUpdateID(t))
		if err != nil || update.State != UpdateCompleted || len(update.Result) == 0 || strings.Contains(string(update.Result), credential) {
			t.Fatalf("short credential remained in declared Update value: %+v %v", update, err)
		}
	}
}

func TestCredentialReplacementCannotHidePEMMarkers(t *testing.T) {
	encoded, _ := json.Marshal(map[string]any{"stage": "-----BEGIN PRIVATE KEY-----\nprivate body\n-----END PRIVATE KEY-----"})
	service := &fakeMessageService{
		query: func(context.Context, *workflowservice.QueryWorkflowRequest) (*workflowservice.QueryWorkflowResponse, error) {
			return &workflowservice.QueryWorkflowResponse{QueryResult: jsonPayloads(encoded)}, nil
		},
		poll: func(_ context.Context, request *workflowservice.PollWorkflowExecutionUpdateRequest) (*workflowservice.PollWorkflowExecutionUpdateResponse, error) {
			return &workflowservice.PollWorkflowExecutionUpdateResponse{UpdateRef: request.UpdateRef, Stage: enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED, Outcome: completedMessageOutcome(string(encoded))}, nil
		},
	}
	bindings := messageBindings()
	bindings[0].Actions[0].ResultSchema = json.RawMessage(queryMessageSchema)
	m, err := NewMessenger(service, "primary", "namespace", bindings, "PRIVATE", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	result, err := m.QueryStatus(t.Context(), pinnedMessageTarget())
	if err != nil || string(result) != `{"stage":"[redacted]"}` {
		t.Fatalf("credential replacement broke PEM redaction: %s %v", result, err)
	}
	update, err := m.LookupAction(t.Context(), pinnedMessageTarget(), "approve_publication", mustUpdateID(t))
	if err != nil || update.State != UpdateCompleted || string(update.Result) != `{"stage":"[redacted]"}` {
		t.Fatalf("credential replacement broke Update PEM redaction: %+v %v", update, err)
	}
}

func TestKnownCredentialBytesInScalarsWithholdResults(t *testing.T) {
	for _, scalar := range []struct{ credential, schemaType, raw string }{
		{"123456", "integer", `{"value":123456}`},
		{"true", "boolean", `{"value":true}`},
		{"null", "null", `{"value":null}`},
	} {
		bindings := messageBindings()
		schema := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"value":{"type":"` + scalar.schemaType + `"}}}`)
		bindings[0].StatusQueryResultSchema = schema
		bindings[0].Actions[0].ResultSchema = schema
		service := &fakeMessageService{
			query: func(context.Context, *workflowservice.QueryWorkflowRequest) (*workflowservice.QueryWorkflowResponse, error) {
				return &workflowservice.QueryWorkflowResponse{QueryResult: jsonPayloads([]byte(scalar.raw))}, nil
			},
			poll: func(_ context.Context, request *workflowservice.PollWorkflowExecutionUpdateRequest) (*workflowservice.PollWorkflowExecutionUpdateResponse, error) {
				return &workflowservice.PollWorkflowExecutionUpdateResponse{UpdateRef: request.UpdateRef, Stage: enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED, Outcome: completedMessageOutcome(scalar.raw)}, nil
			},
		}
		m, err := NewMessenger(service, "primary", "namespace", bindings, scalar.credential, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		query, err := m.QueryStatus(t.Context(), pinnedMessageTarget())
		if err == nil || len(query) != 0 {
			t.Fatalf("credential bytes leaked in Query scalar: %s %v", query, err)
		}
		update, err := m.LookupAction(t.Context(), pinnedMessageTarget(), "approve_publication", mustUpdateID(t))
		if err == nil || update.State != UpdateUnknown || len(update.Result) != 0 {
			t.Fatalf("credential bytes leaked in completed Update scalar: %+v %v", update, err)
		}
	}
}
