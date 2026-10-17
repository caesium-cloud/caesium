package connector

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth"
	core "github.com/caesium-cloud/caesium/internal/connector"
	"github.com/caesium-cloud/caesium/internal/connector/operations"
	"github.com/caesium-cloud/caesium/internal/connector/temporal"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	updatepb "go.temporal.io/api/update/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"google.golang.org/grpc"
)

const querySchema = `{"type":"object","additionalProperties":false,"required":["stage"],"properties":{"stage":{"type":"string"}}}`
const inputSchema = `{"type":"object","additionalProperties":false,"properties":{"note":{"type":"string"}}}`
const resultSchema = `{"type":"object","additionalProperties":false,"required":["approved"],"properties":{"approved":{"type":"boolean"}}}`

type actionRPC struct {
	describeErr error
	workflowservice.WorkflowServiceClient
	sends   atomic.Int64
	queries atomic.Int64
	reads   atomic.Int64
	polls   atomic.Int64
	submit  func(*workflowservice.UpdateWorkflowExecutionRequest) (*workflowservice.UpdateWorkflowExecutionResponse, error)
	poll    func(*workflowservice.PollWorkflowExecutionUpdateRequest) (*workflowservice.PollWorkflowExecutionUpdateResponse, error)
}

func (s *actionRPC) DescribeWorkflowExecution(_ context.Context, r *workflowservice.DescribeWorkflowExecutionRequest, _ ...grpc.CallOption) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	s.reads.Add(1)
	if s.describeErr != nil {
		return nil, s.describeErr
	}
	return &workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Execution: r.Execution, Type: &commonpb.WorkflowType{Name: "publication"}, Status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED}}, nil
}
func (s *actionRPC) QueryWorkflow(_ context.Context, r *workflowservice.QueryWorkflowRequest, _ ...grpc.CallOption) (*workflowservice.QueryWorkflowResponse, error) {
	s.queries.Add(1)
	return &workflowservice.QueryWorkflowResponse{QueryResult: payloads(`{"stage":"closed"}`)}, nil
}
func (s *actionRPC) UpdateWorkflowExecution(_ context.Context, r *workflowservice.UpdateWorkflowExecutionRequest, _ ...grpc.CallOption) (*workflowservice.UpdateWorkflowExecutionResponse, error) {
	s.sends.Add(1)
	return s.submit(r)
}
func (s *actionRPC) PollWorkflowExecutionUpdate(_ context.Context, r *workflowservice.PollWorkflowExecutionUpdateRequest, _ ...grpc.CallOption) (*workflowservice.PollWorkflowExecutionUpdateResponse, error) {
	s.polls.Add(1)
	return s.poll(r)
}
func payloads(raw string) *commonpb.Payloads {
	return &commonpb.Payloads{Payloads: []*commonpb.Payload{{Metadata: map[string][]byte{"encoding": []byte("json/plain")}, Data: []byte(raw)}}}
}
func testActionRuntime(t *testing.T) (*Runtime, *actionRPC, *operations.OperationPrincipal, string) {
	t.Helper()
	r, _ := testRuntime(t)
	bindings := []core.Binding{{Name: "publication", Version: "1", DisplayName: "Publication approval", StatusQuery: "publication_status", StatusQueryResultSchema: json.RawMessage(querySchema), Actions: []core.Action{{Name: "approve_publication", InputSchema: json.RawMessage(inputSchema), ResultSchema: json.RawMessage(resultSchema)}}}}
	r.cfg.Connections[0].Bindings = bindings
	r.connections["primary"].config.Bindings = bindings
	rpc := &actionRPC{}
	m, err := temporal.NewMessenger(rpc, "primary", "default", bindings, "", time.Second, core.MaxPageMetadataBytes)
	require.NoError(t, err)
	r.connections["primary"].messenger = m
	observer, err := temporal.NewObserver(rpc, "default", "", time.Second, 100, 20<<10)
	require.NoError(t, err)
	r.connections["primary"].observer = observer
	ref, err := core.NewExecutionReference("primary", map[string]string{"workflow_id": "workflow-1", "run_id": "run-1"})
	require.NoError(t, err)
	_, err = r.store.RecordSnapshot(t.Context(), core.Observation{Execution: ref, Generation: 1, SourceEventID: "1", NativeStatus: "running", DisplayStatus: core.DisplayRunning, Availability: core.AvailabilityAvailable, Completeness: core.CompletenessComplete, SourceKind: core.SourceDirect, ObservedAt: time.Now()})
	require.NoError(t, err)
	p := &operations.OperationPrincipal{Kind: core.PrincipalKindAPIKey, StableID: uuid.NewString(), Subject: "authenticated-key", Role: "operator"}
	return r, rpc, p, core.ReferenceID(ref)
}
func TestDeclaredQueryIsExplicitBoundedAndSharesRPCSlots(t *testing.T) {
	r, rpc, p, opaque := testActionRuntime(t)
	_, err := r.Detail(t.Context(), principalKey(p), "primary", opaque)
	require.NoError(t, err, "direct detail does not send a status Query")
	require.Zero(t, rpc.queries.Load(), "detail must not issue a status Query")
	for range core.MaxQueriesPerMinute {
		raw, err := r.Query(t.Context(), p, "primary", opaque, "publication_status", "publication", "1")
		require.NoError(t, err)
		require.JSONEq(t, `{"result":{"stage":"closed"}}`, string(raw))
	}
	_, err = r.Query(t.Context(), p, "primary", opaque, "publication_status", "publication", "1")
	require.ErrorIs(t, err, core.ErrReadBudget)
	require.Equal(t, int64(core.MaxQueriesPerMinute), rpc.queries.Load())
	// A distinct principal has a fresh Query budget but must share the existing
	// connection's read/history/action slots, even for a closed workflow.
	other := *p
	other.StableID = uuid.NewString()
	release := make(chan struct{})
	started := make(chan struct{}, core.MaxInFlightRPCs)
	done := make(chan error, core.MaxInFlightRPCs)
	for range core.MaxInFlightRPCs {
		go func() {
			_, e := r.connections["primary"].guard.Do(t.Context(), uuid.NewString(), func(context.Context) ([]byte, error) { started <- struct{}{}; <-release; return nil, nil })
			done <- e
		}()
	}
	for range core.MaxInFlightRPCs {
		<-started
	}
	_, err = r.Query(t.Context(), &other, "primary", opaque, "publication_status", "publication", "1")
	require.ErrorIs(t, err, core.ErrBusy)
	close(release)
	for range core.MaxInFlightRPCs {
		require.NoError(t, <-done)
	}
	require.Equal(t, int64(core.MaxQueriesPerMinute), rpc.queries.Load())
}
func TestPublicOperationsPreserveUnknownAndRequireOriginalRevision(t *testing.T) {
	r, rpc, p, opaque := testActionRuntime(t)
	var firstID string
	rpc.submit = func(request *workflowservice.UpdateWorkflowExecutionRequest) (*workflowservice.UpdateWorkflowExecutionResponse, error) {
		require.Equal(t, "run-1", request.WorkflowExecution.RunId)
		if firstID == "" {
			firstID = request.Request.Meta.UpdateId
		} else {
			require.Equal(t, firstID, request.Request.Meta.UpdateId)
		}
		var envelope struct {
			Input map[string]any     `json:"input"`
			Actor core.ActorEnvelope `json:"_caesium_actor"`
		}
		require.NoError(t, json.Unmarshal(request.Request.Input.Args.Payloads[0].Data, &envelope))
		require.Equal(t, p.StableID, envelope.Actor.StableID)
		require.Equal(t, core.PrincipalKindAPIKey, envelope.Actor.PrincipalKind)
		// The dispatch audit and immutable receipt exist before a send is possible.
		var audit []models.AuditLog
		require.NoError(t, r.db.Where("action = ?", auth.ActionConnectorActionDispatch).Find(&audit).Error)
		require.NotEmpty(t, audit)
		require.Contains(t, string(audit[len(audit)-1].Metadata), envelope.Actor.OperationID)
		require.NotContains(t, string(audit[len(audit)-1].Metadata), "private note")
		return nil, serviceerror.NewUnavailable("lost reply contains private remote error")
	}
	rpc.poll = func(request *workflowservice.PollWorkflowExecutionUpdateRequest) (*workflowservice.PollWorkflowExecutionUpdateResponse, error) {
		require.Equal(t, firstID, request.UpdateRef.UpdateId)
		require.Equal(t, "run-1", request.UpdateRef.WorkflowExecution.RunId)
		return nil, serviceerror.NewNotFound("not proof of rejection")
	}
	submit := func(key string, input map[string]any) (OperationResult, error) {
		return r.Submit(t.Context(), p, "primary", opaque, "approve_publication", key, "publication", "1", input)
	}
	first, err := submit("key-1", map[string]any{"note": "private note"})
	require.ErrorIs(t, err, operations.ErrOperationProvider)
	require.True(t, first.Created)
	require.True(t, first.Pending)
	require.Equal(t, core.OperationUnknown, first.Operation.State)
	raw, err := r.Operations(t.Context(), principalKey(p), "primary", opaque, "", 1)
	require.NoError(t, err)
	require.Contains(t, string(raw), first.Operation.ID.String())
	require.NotContains(t, string(raw), "private note")
	require.NotContains(t, string(raw), "idempotency_key")
	require.NotContains(t, string(raw), "localhost")
	require.Equal(t, int64(1), rpc.sends.Load())
	require.Zero(t, rpc.polls.Load())
	lookup, err := r.Lookup(t.Context(), p, "primary", first.Operation.ID)
	require.ErrorIs(t, err, operations.ErrOperationProvider)
	require.Equal(t, core.OperationUnknown, lookup.Operation.State)
	require.Equal(t, int64(1), rpc.sends.Load())
	other := *p
	other.StableID = uuid.NewString()
	_, err = r.Resubmit(t.Context(), &other, "primary", first.Operation.ID, lookup.Operation.Revision, true)
	require.ErrorIs(t, err, operations.ErrOperationForbidden)
	_, err = r.Resubmit(t.Context(), p, "primary", first.Operation.ID, first.Operation.Revision, true)
	require.ErrorIs(t, err, operations.ErrOperationChanged)
	again, err := r.Resubmit(t.Context(), p, "primary", first.Operation.ID, lookup.Operation.Revision, true)
	require.ErrorIs(t, err, operations.ErrOperationProvider)
	require.Equal(t, uint64(2), again.Operation.Attempt)
	require.Equal(t, int64(2), rpc.sends.Load())
	require.Equal(t, first.Operation.Actor, again.Operation.Actor)
	require.NotEmpty(t, again.Operation.ResendAudit)
	// Different keys/operators recover the one open guard even during outage.
	rpc.describeErr = serviceerror.NewUnavailable("provider offline")
	beforeConflictReads := rpc.reads.Load()
	conflict, err := submit("key-2", map[string]any{"note": "private note"})
	require.NoError(t, err)
	require.True(t, conflict.Conflict)
	require.Equal(t, first.Operation.ID, conflict.Operation.ID)
	require.Equal(t, int64(2), rpc.sends.Load())
	otherConflict, err := r.Submit(t.Context(), &other, "primary", opaque, "approve_publication", "other-key", "publication", "1", map[string]any{"note": "private note"})
	require.NoError(t, err)
	require.True(t, otherConflict.Conflict)
	require.Equal(t, first.Operation.ID, otherConflict.Operation.ID)
	require.Equal(t, beforeConflictReads, rpc.reads.Load(), "open guard recovery cannot require the provider")
	_, err = submit("key-1", map[string]any{"note": "changed"})
	require.ErrorIs(t, err, core.ErrIdempotencyMismatch)
	// A current service with a changed binding refuses recovery before RPC.
	c := r.connections["primary"]
	c.operations = nil
	c.config.Bindings[0].Version = "2"
	before := rpc.reads.Load()
	_, err = r.Lookup(t.Context(), p, "primary", first.Operation.ID)
	require.ErrorIs(t, err, core.ErrInvalidOperation)
	require.Equal(t, before, rpc.reads.Load())
}
func TestInvalidActionsAndEpochMismatchNeverDialOrAdmit(t *testing.T) {
	for _, test := range []struct {
		name    string
		binding string
		input   map[string]any
		want    error
	}{
		{"spoof", "1", map[string]any{core.ReservedActorField: map[string]any{}}, core.ErrInvalidPayload},
		{"schema", "1", map[string]any{"note": 3}, core.ErrInvalidPayload},
		{"stale binding", "old", map[string]any{}, ErrMessageBinding},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, rpc, p, opaque := testActionRuntime(t)
			_, err := r.Submit(t.Context(), p, "primary", opaque, "approve_publication", "key", "publication", test.binding, test.input)
			require.ErrorIs(t, err, test.want)
			require.Zero(t, rpc.sends.Load())
			require.Zero(t, rpc.reads.Load())
			require.Nil(t, r.connections["primary"].client)
			var count int64
			require.NoError(t, r.db.Model(&models.ConnectorOperation{}).Count(&count).Error)
			require.Zero(t, count)
		})
	}
	r, rpc, p, opaque := testActionRuntime(t)
	require.NoError(t, r.store.ActivateConfiguration(t.Context(), r.cfg, r.fingerprint, "new-fingerprint"))
	_, err := r.Submit(t.Context(), p, "primary", opaque, "approve_publication", "key", "publication", "1", map[string]any{})
	require.ErrorIs(t, err, core.ErrEpochMismatch)
	_, err = r.Query(t.Context(), p, "primary", opaque, "publication_status", "publication", "1")
	require.ErrorIs(t, err, core.ErrEpochMismatch)
	require.Zero(t, rpc.reads.Load())
	require.Zero(t, rpc.sends.Load())
	require.Zero(t, rpc.queries.Load())
}
func TestMessageCapabilitiesAndPublicConnectionConfiguration(t *testing.T) {
	r, _, p, _ := testActionRuntime(t)
	r.cfg.Connections[0].UIURL = "https://temporal.example.com/base"
	view, err := r.Connections(t.Context(), principalKey(p))
	require.NoError(t, err)
	require.Equal(t, "https://temporal.example.com/base", view.Connections[0].UIURL)
	require.Equal(t, int64(10000), view.Connections[0].ConsoleRefreshMS)
	require.Contains(t, view.Connections[0].Capabilities, core.CapabilityStatusQuery)
	require.Contains(t, view.Connections[0].Capabilities, core.CapabilityActionSubmission)
	require.Contains(t, view.Connections[0].Capabilities, core.CapabilityReceiptLookup)
	cfg := core.Connection{Bindings: []core.Binding{{Name: "unbound"}}}
	caps := messageCapabilities(cfg)
	require.NotContains(t, caps, core.CapabilityStatusQuery)
	require.NotContains(t, caps, core.CapabilityActionSubmission)
	descriptor := bindingView(r.cfg.Connections[0], "publication", false)
	require.NotNil(t, descriptor)
	require.False(t, descriptor.Available)
	require.False(t, descriptor.Actions[0].Available)
	require.Equal(t, "operator", descriptor.Actions[0].MinimumRole)
	encoded, _ := json.Marshal(view)
	require.False(t, strings.Contains(string(encoded), "endpoint"))
}

func TestReceiptPaginationAndTerminalLookupNeverSend(t *testing.T) {
	r, rpc, p, opaque := testActionRuntime(t)
	rpc.submit = func(request *workflowservice.UpdateWorkflowExecutionRequest) (*workflowservice.UpdateWorkflowExecutionResponse, error) {
		return &workflowservice.UpdateWorkflowExecutionResponse{UpdateRef: &updatepb.UpdateRef{WorkflowExecution: request.WorkflowExecution, UpdateId: request.Request.Meta.UpdateId}, Stage: enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED, Outcome: &updatepb.Outcome{Value: &updatepb.Outcome_Success{Success: payloads(`{"approved":true}`)}}}, nil
	}
	ids := map[uuid.UUID]bool{}
	for range 3 {
		out, err := r.Submit(t.Context(), p, "primary", opaque, "approve_publication", uuid.NewString(), "publication", "1", map[string]any{})
		require.NoError(t, err)
		require.Equal(t, core.OperationCompleted, out.Operation.State)
		ids[out.Operation.ID] = true
	}
	var cursor string
	seen := map[uuid.UUID]bool{}
	for range 3 {
		raw, err := r.Operations(t.Context(), principalKey(p), "primary", opaque, cursor, 1)
		require.NoError(t, err)
		var page struct {
			Operations []Receipt `json:"operations"`
			Cursor     string    `json:"cursor"`
		}
		require.NoError(t, json.Unmarshal(raw, &page))
		require.Len(t, page.Operations, 1)
		id := page.Operations[0].ID
		require.True(t, ids[id])
		require.False(t, seen[id])
		seen[id] = true
		cursor = page.Cursor
		out, err := r.Lookup(t.Context(), p, "primary", id)
		require.NoError(t, err)
		require.Equal(t, core.OperationCompleted, out.Operation.State)
	}
	require.Empty(t, cursor)
	require.Len(t, seen, 3)
	require.Equal(t, int64(3), rpc.sends.Load())
	require.Zero(t, rpc.polls.Load())
	_, err := r.Operations(t.Context(), principalKey(p), "primary", opaque, "tampered.cursor", 1)
	require.ErrorIs(t, err, ErrInvalidRequest)
}

func TestFreshStatelessTargetIsInspectedBeforeAdmission(t *testing.T) {
	r, rpc, p, _ := testActionRuntime(t)
	ref, err := core.NewExecutionReference("primary", map[string]string{"workflow_id": "not-yet-inspected", "run_id": "new-pinned-run"})
	require.NoError(t, err)
	var count int64
	require.NoError(t, r.db.Model(&models.ExternalExecution{}).Where("opaque_id = ?", ref.OpaqueID()).Count(&count).Error)
	require.Zero(t, count)
	rpc.submit = func(request *workflowservice.UpdateWorkflowExecutionRequest) (*workflowservice.UpdateWorkflowExecutionResponse, error) {
		require.NoError(t, r.db.Model(&models.ExternalExecution{}).Where("opaque_id = ?", ref.OpaqueID()).Count(&count).Error)
		require.Equal(t, int64(1), count)
		require.Equal(t, "new-pinned-run", request.WorkflowExecution.RunId)
		return &workflowservice.UpdateWorkflowExecutionResponse{UpdateRef: &updatepb.UpdateRef{WorkflowExecution: request.WorkflowExecution, UpdateId: request.Request.Meta.UpdateId}, Stage: enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_ACCEPTED}, nil
	}
	out, err := r.Submit(t.Context(), p, "primary", core.ReferenceID(ref), "approve_publication", "fresh-key", "publication", "1", map[string]any{})
	require.NoError(t, err)
	require.True(t, out.Created)
	require.Equal(t, core.OperationAccepted, out.Operation.State)
	require.Equal(t, int64(1), rpc.sends.Load())
	require.GreaterOrEqual(t, rpc.reads.Load(), int64(2), "composition observes the fresh reference and adapter verifies it immediately before send")
}
func TestCachedTargetCannotAuthorizeNewActionDuringOutage(t *testing.T) {
	r, rpc, p, opaque := testActionRuntime(t)
	rpc.describeErr = serviceerror.NewUnavailable("private outage")
	_, err := r.Submit(t.Context(), p, "primary", opaque, "approve_publication", "new-key", "publication", "1", map[string]any{})
	require.Error(t, err)
	require.Zero(t, rpc.sends.Load())
	var count int64
	require.NoError(t, r.db.Model(&models.ConnectorOperation{}).Count(&count).Error)
	require.Zero(t, count)
}
func TestActionRecoveryEnvelopeIsReservedBeforeDispatch(t *testing.T) {
	r, rpc, p, opaque := testActionRuntime(t)
	c := r.connections["primary"]
	c.config.Limits.MaxPageMetadataBytes = 1000
	_, err := r.Submit(t.Context(), p, "primary", opaque, "approve_publication", "small-key", "publication", "1", map[string]any{})
	require.ErrorIs(t, err, core.ErrPayloadTooLarge)
	require.Zero(t, rpc.sends.Load())
	require.Zero(t, rpc.reads.Load())
	var count int64
	require.NoError(t, r.db.Model(&models.ConnectorOperation{}).Count(&count).Error)
	require.Zero(t, count)
	c.config.Limits.MaxPageMetadataBytes = 2000
	rpc.submit = func(request *workflowservice.UpdateWorkflowExecutionRequest) (*workflowservice.UpdateWorkflowExecutionResponse, error) {
		return &workflowservice.UpdateWorkflowExecutionResponse{UpdateRef: &updatepb.UpdateRef{WorkflowExecution: request.WorkflowExecution, UpdateId: request.Request.Meta.UpdateId}, Stage: enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED, Outcome: &updatepb.Outcome{Value: &updatepb.Outcome_Success{Success: payloads(`{"approved":true}`)}}}, nil
	}
	out, err := r.Submit(t.Context(), p, "primary", opaque, "approve_publication", "larger-key", "publication", "1", map[string]any{})
	require.NoError(t, err)
	require.Equal(t, int64(1), rpc.sends.Load())
	op, err := operations.NewStore(r.db).Operation(t.Context(), out.Operation.ID)
	require.NoError(t, err)
	// Reduce the presentation ceiling after completion: lookup remains useful
	// through the explicit compact view and retains confirmation coordinates.
	compact, err := receipt(op)
	require.NoError(t, err)
	compact.compact = true
	minimum, err := json.Marshal(OperationResult{Operation: compact})
	require.NoError(t, err)
	c.config.Limits.MaxPageMetadataBytes = len(minimum)
	recovered, err := r.Lookup(t.Context(), p, "primary", op.ID)
	require.NoError(t, err)
	raw, err := json.Marshal(recovered)
	require.NoError(t, err)
	require.LessOrEqual(t, len(raw), len(minimum))
	require.Contains(t, string(raw), `"metadata_omitted":true`)
	require.Contains(t, string(raw), op.ExternalUpdateID)
	require.Contains(t, string(raw), opaque)
	require.Contains(t, string(raw), p.StableID)
	require.Contains(t, string(raw), `"binding_version":"1"`)
	require.NotContains(t, string(raw), `"configuration_epoch"`)
	require.Equal(t, int64(1), rpc.sends.Load())
	require.Zero(t, rpc.polls.Load())
}

func TestCompactOperationPagesRecoverAllIDsAfterResendAuditGrowth(t *testing.T) {
	r, rpc, p, opaque := testActionRuntime(t)
	rpc.submit = func(request *workflowservice.UpdateWorkflowExecutionRequest) (*workflowservice.UpdateWorkflowExecutionResponse, error) {
		return nil, serviceerror.NewUnavailable("lost reply")
	}
	first, err := r.Submit(t.Context(), p, "primary", opaque, "approve_publication", "audit-growth", "publication", "1", map[string]any{})
	require.ErrorIs(t, err, operations.ErrOperationProvider)
	for range 15 {
		first, err = r.Resubmit(t.Context(), p, "primary", first.Operation.ID, first.Operation.Revision, true)
		require.ErrorIs(t, err, operations.ErrOperationProvider)
	}
	rpc.submit = func(request *workflowservice.UpdateWorkflowExecutionRequest) (*workflowservice.UpdateWorkflowExecutionResponse, error) {
		return &workflowservice.UpdateWorkflowExecutionResponse{UpdateRef: &updatepb.UpdateRef{WorkflowExecution: request.WorkflowExecution, UpdateId: request.Request.Meta.UpdateId}, Stage: enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED, Outcome: &updatepb.Outcome{Value: &updatepb.Outcome_Success{Success: payloads(`{"approved":true}`)}}}, nil
	}
	first, err = r.Resubmit(t.Context(), p, "primary", first.Operation.ID, first.Operation.Revision, true)
	require.NoError(t, err)
	require.Equal(t, uint64(17), first.Operation.Attempt)
	require.Greater(t, len(first.Operation.ResendAudit), 1600)
	expected := map[uuid.UUID]string{first.Operation.ID: first.Operation.ExternalUpdateID}
	for range 2 {
		out, err := r.Submit(t.Context(), p, "primary", opaque, "approve_publication", uuid.NewString(), "publication", "1", map[string]any{})
		require.NoError(t, err)
		expected[out.Operation.ID] = out.Operation.ExternalUpdateID
	}
	r.connections["primary"].config.Limits.MaxPageMetadataBytes = 1600
	seen := map[uuid.UUID]bool{}
	cursor := ""
	for range 3 {
		raw, err := r.Operations(t.Context(), principalKey(p), "primary", opaque, cursor, 1)
		require.NoError(t, err)
		require.LessOrEqual(t, len(raw), 1600)
		var page struct {
			Operations []struct {
				Receipt
				MetadataOmitted bool `json:"metadata_omitted"`
			} `json:"operations"`
			Cursor string `json:"cursor"`
		}
		require.NoError(t, json.Unmarshal(raw, &page))
		require.Len(t, page.Operations, 1)
		op := page.Operations[0]
		require.False(t, seen[op.ID])
		require.Contains(t, expected, op.ID)
		require.Equal(t, expected[op.ID], op.ExternalUpdateID)
		require.Equal(t, opaque, op.ExecutionID)
		require.Equal(t, p.StableID, op.Actor.StableID)
		require.Equal(t, p.Kind, op.Actor.PrincipalKind)
		require.Equal(t, "1", op.BindingVersion)
		require.Equal(t, core.OperationCompleted, op.State)
		if op.ID == first.Operation.ID {
			require.True(t, op.MetadataOmitted)
			require.Equal(t, uint64(17), op.Attempt)
		}
		seen[op.ID] = true
		cursor = page.Cursor
	}
	require.Empty(t, cursor)
	require.Len(t, seen, 3)
}
