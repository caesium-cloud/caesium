package temporal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/connector"
	"github.com/santhosh-tekuri/jsonschema/v6"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	querypb "go.temporal.io/api/query/v1"
	"go.temporal.io/api/serviceerror"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	updatepb "go.temporal.io/api/update/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MessageService is the raw RPC surface needed for declared messages. Using
// this surface avoids custom SDK data converters: v1 accepts json/plain only.
type MessageService interface {
	DescribeWorkflowExecution(context.Context, *workflowservice.DescribeWorkflowExecutionRequest, ...grpc.CallOption) (*workflowservice.DescribeWorkflowExecutionResponse, error)
	DescribeTaskQueue(context.Context, *workflowservice.DescribeTaskQueueRequest, ...grpc.CallOption) (*workflowservice.DescribeTaskQueueResponse, error)
	QueryWorkflow(context.Context, *workflowservice.QueryWorkflowRequest, ...grpc.CallOption) (*workflowservice.QueryWorkflowResponse, error)
	UpdateWorkflowExecution(context.Context, *workflowservice.UpdateWorkflowExecutionRequest, ...grpc.CallOption) (*workflowservice.UpdateWorkflowExecutionResponse, error)
	PollWorkflowExecutionUpdate(context.Context, *workflowservice.PollWorkflowExecutionUpdateRequest, ...grpc.CallOption) (*workflowservice.PollWorkflowExecutionUpdateResponse, error)
}

var _ MessageService = workflowservice.WorkflowServiceClient(nil)

var messagePEM = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]+-----.*?-----END [A-Z0-9 ]+-----`)

// MessageTarget pins the connection, execution run, and binding revision.
// BindingName is the exact Temporal workflow type in v1. Empty RunID never
// means latest: it is refused before any RPC.
type MessageTarget struct {
	ConnectionID   string
	WorkflowID     string
	RunID          string
	BindingName    string
	BindingVersion string
}

const (
	UpdateSubmitted = "submitted"
	UpdateAccepted  = "accepted"
	UpdateCompleted = "completed"
	UpdateRejected  = "rejected"
	UpdateUnknown   = "unknown"
)

// UpdateResult is provider evidence, not a durable receipt. RPCCode is the
// exact gRPC code of a failed RPC. FailureKind is bounded metadata; arbitrary
// remote failure messages/details and payloads are never exposed. A failed
// outcome establishes remote rejection, while an RPC error never does.
type UpdateResult struct {
	State       string          `json:"state"`
	NativeStage string          `json:"native_stage,omitempty"`
	RPCCode     string          `json:"rpc_code,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	FailureKind string          `json:"failure_kind,omitempty"`
}

// WorkerObservation reports recent task-queue poller evidence. No recent
// pollers is a missing-worker diagnostic, not proof that no worker exists now.
// Conversely, a recent poller does not prove present worker liveness.
type WorkerObservation struct {
	Availability string `json:"availability"`
	RPCCode      string `json:"rpc_code,omitempty"`
}

type messageBinding struct {
	binding connector.Binding
	query   *jsonschema.Schema
	actions map[string]messageAction
}

type messageAction struct {
	action connector.Action
	input  *jsonschema.Schema
	result *jsonschema.Schema
}

// Messenger reads status and sends declared Updates within one configured
// connection. It neither stores receipts nor retries/resubmits messages.
// Authorization, budgets, epoch policy and admission-before-send belong to
// the service composing this adapter. actor must come from its authenticated
// principal, never from the caller's JSON or the connector service identity.
type Messenger struct {
	service         MessageService
	connectionID    string
	namespace       string
	apiKey          string
	bindings        map[string]messageBinding
	deadline        time.Duration
	maxPayloadBytes int
}

// NewMessenger freezes bindings and compiles their schemas. A missing status
// result schema keeps old configuration parseable but disables its Query.
// maxPayloadBytes defaults to 64 KiB and may only be smaller. apiKey is used
// to redact constructor errors and validated results; RPC errors expose codes,
// never remote text.
func NewMessenger(service MessageService, connectionID, namespace string, bindings []connector.Binding, apiKey string, deadline time.Duration, maxPayloadBytes int) (*Messenger, error) {
	fail := func(err error) (*Messenger, error) { return nil, redactError(err, []string{apiKey}) }
	if service == nil || strings.TrimSpace(connectionID) == "" || strings.TrimSpace(namespace) == "" {
		return fail(errors.New("temporal message service, connection id, and namespace are required"))
	}
	if deadline == 0 {
		deadline = connector.MaxRPCDeadline
	}
	if deadline < 0 || deadline > connector.MaxRPCDeadline {
		return fail(errors.New("temporal message deadline is outside the configured ceiling"))
	}
	if maxPayloadBytes == 0 {
		maxPayloadBytes = connector.MaxPageMetadataBytes
	}
	if maxPayloadBytes < 1 || maxPayloadBytes > connector.MaxPageMetadataBytes {
		return fail(errors.New("temporal message payload budget is outside the configured ceiling"))
	}
	m := &Messenger{service: service, connectionID: connectionID, namespace: namespace, apiKey: apiKey,
		bindings: make(map[string]messageBinding, len(bindings)), deadline: deadline, maxPayloadBytes: maxPayloadBytes}
	for _, binding := range bindings {
		if strings.TrimSpace(binding.Name) == "" || strings.TrimSpace(binding.Version) == "" || strings.TrimSpace(binding.StatusQuery) == "" {
			return fail(errors.New("temporal message binding requires name, version, and status query"))
		}
		if _, exists := m.bindings[binding.Name]; exists {
			return fail(errors.New("duplicate temporal message binding"))
		}
		// Caller mutations after construction cannot change dispatch or schemas.
		binding.StatusQueryResultSchema = bytes.Clone(binding.StatusQueryResultSchema)
		binding.Actions = append([]connector.Action(nil), binding.Actions...)
		compiled := messageBinding{binding: binding, actions: make(map[string]messageAction, len(binding.Actions))}
		var err error
		if len(binding.StatusQueryResultSchema) > 0 {
			compiled.query, err = compileMessageSchema(binding.StatusQueryResultSchema)
			if err != nil {
				return fail(err)
			}
		}
		for _, action := range binding.Actions {
			if strings.TrimSpace(action.Name) == "" {
				return fail(errors.New("temporal action name is required"))
			}
			if _, exists := compiled.actions[action.Name]; exists {
				return fail(errors.New("duplicate temporal action"))
			}
			action.InputSchema = bytes.Clone(action.InputSchema)
			action.ResultSchema = bytes.Clone(action.ResultSchema)
			input, err := compileMessageSchema(action.InputSchema)
			if err != nil {
				return fail(err)
			}
			result, err := compileMessageSchema(action.ResultSchema)
			if err != nil {
				return fail(err)
			}
			compiled.actions[action.Name] = messageAction{action: action, input: input, result: result}
		}
		m.bindings[binding.Name] = compiled
	}
	return m, nil
}

// NewMessengerFromClient reuses the SDK transport, without its payload codec.
func NewMessengerFromClient(c client.Client, connectionID, namespace string, bindings []connector.Binding, apiKey string, deadline time.Duration, maxPayloadBytes int) (*Messenger, error) {
	if c == nil {
		return nil, errors.New("temporal client is required")
	}
	return NewMessenger(c.WorkflowService(), connectionID, namespace, bindings, apiKey, deadline, maxPayloadBytes)
}

// StableUpdateID deterministically names the Update for a persisted operation.
// It is never generated during dispatch or lookup. D2 must persist this id and
// immutable target before calling SubmitAction.
func StableUpdateID(operationID string) (string, error) {
	if strings.TrimSpace(operationID) == "" {
		return "", errors.New("operation id is required")
	}
	sum := sha256.Sum256([]byte("caesium-connector-update-v1\x00" + operationID))
	return "caesium-" + hex.EncodeToString(sum[:]), nil
}

// QueryStatus invokes exactly the binding's declared no-argument Query once.
// Closed workflows are not automatically queried or replayed by this package.
func (m *Messenger) QueryStatus(ctx context.Context, target MessageTarget) (json.RawMessage, error) {
	binding, err := m.binding(target)
	if err != nil {
		return nil, err
	}
	if binding.query == nil {
		return nil, errors.New("status Query requires a declared result schema")
	}
	ctx, cancel := m.rpcContext(ctx)
	defer cancel()
	if _, err := m.inspectTarget(ctx, target); err != nil {
		return nil, err
	}
	response, err := m.service.QueryWorkflow(ctx, &workflowservice.QueryWorkflowRequest{
		Namespace: m.namespace, Execution: workflowExecution(target),
		Query: &querypb.WorkflowQuery{QueryType: binding.binding.StatusQuery},
	})
	if err != nil {
		return nil, messageRPCError(err)
	}
	if response == nil || response.GetQueryRejected() != nil {
		return nil, errors.New("temporal status Query was unavailable or rejected")
	}
	return m.decodeResult(response.GetQueryResult(), binding.query)
}

// SubmitAction validates user JSON, stamps the authenticated server actor and
// sends one Update pinned to the specified run. It waits for acceptance, not
// completion. An outcome already returned by the server may prove completion
// or rejection. There is no automatic retry even when a reply is lost.
func (m *Messenger) SubmitAction(ctx context.Context, target MessageTarget, actionName, updateID string, input map[string]any, actor connector.ActorEnvelope) (UpdateResult, error) {
	action, err := m.action(target, actionName, updateID)
	if err != nil {
		return UpdateResult{}, err
	}
	if err := actor.Validate(); err != nil {
		return UpdateResult{}, err
	}
	if actor.Role != "operator" && actor.Role != "admin" {
		return UpdateResult{}, errors.New("temporal action requires an operator or admin actor")
	}
	if actor.BindingVersion != target.BindingVersion {
		return UpdateResult{}, errors.New("actor binding version does not match the pinned target")
	}
	expected, _ := StableUpdateID(actor.OperationID)
	if updateID != expected {
		return UpdateResult{}, errors.New("update id does not match the persisted actor operation")
	}
	if _, spoof := input[connector.ReservedActorField]; spoof {
		return UpdateResult{}, errors.New("caller cannot supply the reserved actor field")
	}
	if input == nil {
		input = map[string]any{}
	}
	encoded, err := json.Marshal(input)
	if err != nil || len(encoded) > m.maxPayloadBytes {
		return UpdateResult{}, errors.New("action input is invalid JSON or exceeds the payload budget")
	}
	validated, err := validateMessageJSON(encoded, action.input)
	if err != nil {
		return UpdateResult{}, err
	}
	// Separate caller input from the immutable reserved context. This envelope
	// is the only argument sent to the remote Update handler.
	envelope, err := connector.ApplyActor(map[string]any{"input": validated}, actor)
	if err != nil {
		return UpdateResult{}, err
	}
	encoded, err = json.Marshal(envelope)
	if err != nil || len(encoded) > m.maxPayloadBytes {
		return UpdateResult{}, errors.New("stamped action input exceeds the payload budget")
	}
	ctx, cancel := m.rpcContext(ctx)
	defer cancel()
	if _, err := m.inspectTarget(ctx, target); err != nil {
		return unknownRPC(err)
	}
	response, err := m.service.UpdateWorkflowExecution(ctx, &workflowservice.UpdateWorkflowExecutionRequest{
		Namespace: m.namespace, WorkflowExecution: workflowExecution(target),
		WaitPolicy: &updatepb.WaitPolicy{LifecycleStage: enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_ACCEPTED},
		Request: &updatepb.Request{
			Meta:  &updatepb.Meta{UpdateId: updateID, Identity: "caesium-connector"},
			Input: &updatepb.Input{Name: actionName, Args: jsonPayloads(encoded)},
		},
	})
	if err != nil {
		return unknownRPC(err)
	}
	if response == nil || !sameUpdateRef(response.GetUpdateRef(), target, updateID) {
		return UpdateResult{State: UpdateUnknown}, errors.New("temporal Update response did not match the pinned target")
	}
	return m.updateResult(response.GetStage(), response.GetOutcome(), action.result)
}

// LookupAction makes one non-blocking poll of the same Update id/run. It has
// no input or actor argument and cannot resend. In particular NotFound after
// an uncertain rejection is unknown: rejected-before-acceptance Updates need
// not be in history. Absence never authorizes resubmission or releases a guard.
func (m *Messenger) LookupAction(ctx context.Context, target MessageTarget, actionName, updateID string) (UpdateResult, error) {
	action, err := m.action(target, actionName, updateID)
	if err != nil {
		return UpdateResult{}, err
	}
	ctx, cancel := m.rpcContext(ctx)
	defer cancel()
	if _, err := m.inspectTarget(ctx, target); err != nil {
		return unknownRPC(err)
	}
	response, err := m.service.PollWorkflowExecutionUpdate(ctx, &workflowservice.PollWorkflowExecutionUpdateRequest{
		Namespace: m.namespace, Identity: "caesium-connector",
		UpdateRef: &updatepb.UpdateRef{WorkflowExecution: workflowExecution(target), UpdateId: updateID},
		// No WaitPolicy means non-blocking. No loop or Update RPC follows.
	})
	if err != nil {
		return unknownRPC(err)
	}
	if response == nil || !sameUpdateRef(response.GetUpdateRef(), target, updateID) {
		return UpdateResult{State: UpdateUnknown}, errors.New("temporal lookup response did not match the pinned target")
	}
	return m.updateResult(response.GetStage(), response.GetOutcome(), action.result)
}

// CheckWorker is an explicit diagnostic, never invoked by Query, submission,
// or receipt lookup. It reports the server's recent workflow poller evidence
// without exposing worker identities or inferring rejection from a timeout.
func (m *Messenger) CheckWorker(ctx context.Context, target MessageTarget) (WorkerObservation, error) {
	if _, err := m.binding(target); err != nil {
		return WorkerObservation{}, err
	}
	ctx, cancel := m.rpcContext(ctx)
	defer cancel()
	describe, err := m.inspectTarget(ctx, target)
	if err != nil {
		return WorkerObservation{Availability: "unknown"}, err
	}
	queue := describe.GetExecutionConfig().GetTaskQueue().GetName()
	if queue == "" {
		return WorkerObservation{Availability: "unknown"}, errors.New("temporal execution has no task queue evidence")
	}
	response, err := m.service.DescribeTaskQueue(ctx, &workflowservice.DescribeTaskQueueRequest{
		Namespace: m.namespace, TaskQueue: &taskqueuepb.TaskQueue{Name: queue}, TaskQueueType: enumspb.TASK_QUEUE_TYPE_WORKFLOW,
	})
	if err != nil {
		return WorkerObservation{Availability: "unknown", RPCCode: rpcCode(err).String()}, messageRPCError(err)
	}
	if response == nil {
		return WorkerObservation{Availability: "unknown"}, errors.New("temporal returned no task queue evidence")
	}
	availability := "no_recent_pollers"
	for _, poller := range response.GetPollers() {
		if poller != nil {
			availability = "recent_pollers"
			break
		}
	}
	return WorkerObservation{Availability: availability}, nil
}

func (m *Messenger) binding(target MessageTarget) (messageBinding, error) {
	if target.ConnectionID != m.connectionID || strings.TrimSpace(target.WorkflowID) == "" || strings.TrimSpace(target.RunID) == "" {
		return messageBinding{}, errors.New("message target requires the configured connection and pinned WorkflowID/RunID")
	}
	binding, ok := m.bindings[target.BindingName]
	if !ok || target.BindingVersion != binding.binding.Version {
		return messageBinding{}, errors.New("message target binding is undeclared or has changed")
	}
	return binding, nil
}

func (m *Messenger) action(target MessageTarget, name, updateID string) (messageAction, error) {
	binding, err := m.binding(target)
	if err != nil {
		return messageAction{}, err
	}
	action, ok := binding.actions[name]
	if !ok {
		return messageAction{}, errors.New("temporal action handler is not binding-declared")
	}
	if strings.TrimSpace(updateID) == "" {
		return messageAction{}, errors.New("persisted Update id is required")
	}
	return action, nil
}

func (m *Messenger) inspectTarget(ctx context.Context, target MessageTarget) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	response, err := m.service.DescribeWorkflowExecution(ctx, &workflowservice.DescribeWorkflowExecutionRequest{
		Namespace: m.namespace, Execution: workflowExecution(target),
	})
	if err != nil {
		return nil, messageRPCError(err)
	}
	info := response.GetWorkflowExecutionInfo()
	if info.GetExecution().GetWorkflowId() != target.WorkflowID || info.GetExecution().GetRunId() != target.RunID || info.GetType().GetName() != target.BindingName {
		return nil, errors.New("temporal execution identity or workflow type does not match the pinned binding")
	}
	return response, nil
}

func (m *Messenger) rpcContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, m.deadline)
}

func (m *Messenger) updateResult(stage enumspb.UpdateWorkflowExecutionLifecycleStage, outcome *updatepb.Outcome, schema *jsonschema.Schema) (UpdateResult, error) {
	result := UpdateResult{State: UpdateUnknown, NativeStage: enumspb.UpdateWorkflowExecutionLifecycleStage_name[int32(stage)]}
	if outcome != nil {
		if stage != enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED {
			return result, errors.New("temporal returned an outcome before completion")
		}
		switch value := outcome.GetValue().(type) {
		case *updatepb.Outcome_Failure:
			if value.Failure == nil {
				return result, errors.New("temporal returned an empty failure outcome")
			}
			result.State, result.FailureKind = UpdateRejected, "remote_update_failure"
			return result, nil
		case *updatepb.Outcome_Success:
			decoded, err := m.decodeResult(value.Success, schema)
			if err != nil {
				// The provider completed, but no valid result can be exposed.
				// Keep the operation unknown so it cannot authorize another effect.
				return result, err
			}
			result.State, result.Result = UpdateCompleted, decoded
			return result, nil
		default:
			return result, errors.New("temporal returned an unsupported outcome")
		}
	}
	switch stage {
	case enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_ADMITTED:
		result.State = UpdateSubmitted
	case enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_ACCEPTED:
		result.State = UpdateAccepted
	case enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED:
		return result, errors.New("temporal completion has no outcome")
	}
	return result, nil
}

func (m *Messenger) decodeResult(payloads *commonpb.Payloads, schema *jsonschema.Schema) (json.RawMessage, error) {
	if payloads == nil || len(payloads.GetPayloads()) != 1 || payloads.Payloads[0] == nil {
		return nil, errors.New("temporal result must be one default JSON payload")
	}
	payload := payloads.Payloads[0]
	if string(payload.GetMetadata()["encoding"]) != "json/plain" || len(payload.GetMetadata()) != 1 {
		return nil, errors.New("temporal custom or encrypted result payload is unavailable")
	}
	if len(payload.GetData()) > m.maxPayloadBytes {
		return nil, errors.New("temporal result exceeds the payload budget")
	}
	instance, err := validateMessageJSON(payload.GetData(), schema)
	if err != nil {
		return nil, err
	}
	if err := m.redactObject(instance); err != nil {
		return nil, err
	}
	if err := schema.Validate(instance); err != nil {
		return nil, errors.New("redacted Temporal result does not match its declared schema")
	}
	// Re-encode a validated object rather than returning arbitrary wire bytes.
	encoded, err := json.Marshal(instance)
	if err != nil || len(encoded) > m.maxPayloadBytes {
		return nil, errors.New("temporal validated result exceeds the payload budget")
	}
	if m.apiKey != "" && bytes.Contains(encoded, []byte(m.apiKey)) {
		// A credential can be numeric, boolean, or null text, not only a
		// string. Withhold rather than change a schema-validated scalar.
		return nil, errors.New("temporal result contains credential bytes")
	}
	return encoded, nil
}

func workflowExecution(target MessageTarget) *commonpb.WorkflowExecution {
	return &commonpb.WorkflowExecution{WorkflowId: target.WorkflowID, RunId: target.RunID}
}

func sameUpdateRef(ref *updatepb.UpdateRef, target MessageTarget, id string) bool {
	return ref.GetUpdateId() == id && ref.GetWorkflowExecution().GetWorkflowId() == target.WorkflowID && ref.GetWorkflowExecution().GetRunId() == target.RunID
}

func jsonPayloads(encoded []byte) *commonpb.Payloads {
	return &commonpb.Payloads{Payloads: []*commonpb.Payload{{Metadata: map[string][]byte{"encoding": []byte("json/plain")}, Data: encoded}}}
}

func rpcCode(err error) codes.Code {
	if errors.Is(err, context.DeadlineExceeded) {
		return codes.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return codes.Canceled
	}
	return serviceerror.ToStatus(err).Code()
}

func messageRPCError(err error) error {
	// Never echo arbitrary remote text, payloads, credentials, or actor fields.
	return status.Error(rpcCode(err), "temporal message RPC unavailable")
}

func (m *Messenger) redactObject(object map[string]any) error {
	for key, value := range object {
		if (m.apiKey != "" && strings.Contains(key, m.apiKey)) || messagePEM.MatchString(key) {
			// Withhold the entire result. Replacing a key could collide with
			// existing data, and changing keys while ranging a map can skip
			// or revisit newly inserted keys.
			return errors.New("temporal result contains a sensitive object key")
		}
		redacted, err := m.redactValue(value)
		if err != nil {
			return err
		}
		object[key] = redacted
	}
	return nil
}

func (m *Messenger) redactValue(value any) (any, error) {
	switch typed := value.(type) {
	case string:
		return m.redactString(typed), nil
	case map[string]any:
		if err := m.redactObject(typed); err != nil {
			return nil, err
		}
	case []any:
		for i, child := range typed {
			redacted, err := m.redactValue(child)
			if err != nil {
				return nil, err
			}
			typed[i] = redacted
		}
	}
	return value, nil
}

func (m *Messenger) redactString(value string) string {
	replacement := "[redacted]"
	if m.apiKey != "" {
		// Payloads must not use the generic error redactor's short-secret
		// threshold. Even a short known credential must be removed. Avoid
		// reintroducing a credential that happens to occur in the placeholder.
		if strings.Contains(replacement, m.apiKey) {
			replacement = ""
		}
	}
	// Match the PEM block before replacing credentials: a credential could
	// be a substring of BEGIN/END markers, and removing it first would hide
	// the block from the matcher while leaving its body exposed.
	value = messagePEM.ReplaceAllString(value, replacement)
	if m.apiKey != "" {
		value = strings.ReplaceAll(value, m.apiKey, replacement)
	}
	return value
}

func unknownRPC(err error) (UpdateResult, error) {
	return UpdateResult{State: UpdateUnknown, RPCCode: rpcCode(err).String()}, messageRPCError(err)
}

type messageSchemaLoader struct{}

func (messageSchemaLoader) Load(string) (any, error) {
	return nil, errors.New("external message schema references are not allowed")
}

func compileMessageSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, errors.New("message schema is invalid JSON")
	}
	root, ok := doc.(map[string]any)
	if !ok || root["type"] != "object" || root["additionalProperties"] != false {
		return nil, errors.New("message schema must be a closed JSON object schema")
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(messageSchemaLoader{})
	const resource = "https://caesium.local/temporal-message-schema.json"
	if err := compiler.AddResource(resource, doc); err != nil {
		return nil, errors.New("message schema is invalid")
	}
	schema, err := compiler.Compile(resource)
	if err != nil {
		return nil, errors.New("message schema is invalid")
	}
	return schema, nil
}

func validateMessageJSON(raw []byte, schema *jsonschema.Schema) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, errors.New("message payload must be a JSON object")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("message payload has trailing JSON")
	}
	if err := schema.Validate(object); err != nil {
		return nil, errors.New("message payload does not match its declared schema")
	}
	return object, nil
}
