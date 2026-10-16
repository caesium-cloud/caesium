package temporal

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/connector"
	coperations "github.com/caesium-cloud/caesium/internal/connector/operations"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestOperationProviderLookupCannotSendAndPreservesNotFound(t *testing.T) {
	service := &fakeMessageService{poll: func(_ context.Context, r *workflowservice.PollWorkflowExecutionUpdateRequest) (*workflowservice.PollWorkflowExecutionUpdateResponse, error) {
		require.Equal(t, "original-run", r.UpdateRef.WorkflowExecution.RunId)
		require.Equal(t, "publication-workflow", r.UpdateRef.WorkflowExecution.WorkflowId)
		require.Equal(t, mustUpdateID(t), r.UpdateRef.UpdateId)
		return nil, serviceerror.NewNotFound("ambiguous provider absence")
	}}
	p := &OperationProvider{Messenger: mustMessenger(t, service)}
	ref, e := connector.NewExecutionReference("primary", map[string]string{"workflow_id": "publication-workflow", "run_id": "original-run"})
	require.NoError(t, e)
	d := coperations.OperationDispatch{Target: coperations.OperationTarget{Execution: ref, BindingName: "publication", BindingVersion: "1"}, Action: "approve_publication", ExternalUpdateID: mustUpdateID(t)}
	evidence, e := p.LookupOperation(t.Context(), d)
	require.Error(t, e)
	require.Equal(t, connector.OperationUnknown, evidence.State)
	require.Equal(t, "NotFound", evidence.RPCCode)
	require.Empty(t, evidence.RejectionProof)
	require.Equal(t, 1, service.polls)
	require.Zero(t, service.sends)
	// Extra or missing coordinates cannot be interpreted as a latest-run action.
	d.Target.Execution.Coordinates["unexpected"] = "coordinate"
	evidence, e = p.LookupOperation(t.Context(), d)
	require.Error(t, e)
	require.Equal(t, connector.OperationNotDispatched, evidence.State)
	require.Equal(t, 1, service.polls)
	require.Zero(t, service.sends)
}

func operationBridgeService(t *testing.T, raw *fakeMessageService, maxBytes int, resultSchema string) (*coperations.Service, *OperationProvider, coperations.OperationSubmission) {
	t.Helper()
	bindings := messageBindings()
	if resultSchema != "" {
		bindings[0].Actions[0].ResultSchema = json.RawMessage(resultSchema)
	}
	connection := connector.Connection{ID: "primary", Enabled: true, Provider: connector.ProviderTemporal, Endpoint: "localhost:7233", Scope: "namespace", Bindings: bindings, Limits: connector.DefaultLimits()}
	if maxBytes > 0 {
		connection.Limits.MaxPageMetadataBytes = maxBytes
	}
	messenger, e := NewMessenger(raw, "primary", "namespace", bindings, "", 0, maxBytes)
	require.NoError(t, e)
	provider := &OperationProvider{Messenger: messenger}
	db, e := gorm.Open(sqlite.Open("file:"+filepath.Join(t.TempDir(), "bridge.db")+"?_journal_mode=WAL&_busy_timeout=5000&_txlock=immediate"), &gorm.Config{})
	require.NoError(t, e)
	sqlDB, e := db.DB()
	require.NoError(t, e)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&models.ExternalExecution{}, &models.ExternalExecutionSnapshot{}, &models.ConnectorOperation{}, &models.ConnectorConfiguration{}))
	catalog := connector.NewStore(db)
	require.NoError(t, catalog.ActivateEpoch(t.Context(), "", "epoch"))
	ref, e := connector.NewExecutionReference("primary", map[string]string{"workflow_id": "publication-workflow", "run_id": "original-run"})
	require.NoError(t, e)
	_, e = catalog.RecordSnapshot(t.Context(), connector.Observation{Execution: ref, Generation: 1, SourceEventID: "direct", NativeStatus: "running", DisplayStatus: connector.DisplayRunning, Availability: connector.AvailabilityAvailable, Completeness: connector.CompletenessComplete, SourceKind: connector.SourceDirect, ObservedAt: time.Now().UTC()})
	require.NoError(t, e)
	service, e := coperations.NewService(coperations.NewStore(db), provider, connection, "epoch")
	require.NoError(t, e)
	request := coperations.OperationSubmission{IdempotencyKey: "bridge-key", Target: coperations.OperationTarget{Execution: ref, BindingName: "publication", BindingVersion: "1"}, Action: "approve_publication", Input: map[string]any{"note": "reviewed"}, Principal: &coperations.OperationPrincipal{Kind: connector.PrincipalKindAPIKey, StableID: uuid.NewString(), Subject: "authenticated-key", Role: "operator"}}
	return service, provider, request
}

func TestOperationServiceBridgeTransientPreflightRetriesOnlyProvenUnsent(t *testing.T) {
	for _, code := range []codes.Code{codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.Unknown} {
		t.Run(code.String(), func(t *testing.T) {
			raw := &fakeMessageService{describe: func(context.Context, *workflowservice.DescribeWorkflowExecutionRequest) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
				return nil, status.Error(code, "preflight unavailable")
			}}
			service, _, request := operationBridgeService(t, raw, 0, "")
			first, e := service.Submit(t.Context(), request)
			require.ErrorIs(t, e, coperations.ErrOperationProvider)
			require.Equal(t, connector.OperationSubmitted, first.Operation.State)
			require.True(t, first.Operation.RetryableUnsent)
			require.NotNil(t, first.Operation.OpenKey)
			require.Zero(t, raw.sends)
			require.Zero(t, raw.polls)
			raw.describe = nil
			raw.submit = func(_ context.Context, r *workflowservice.UpdateWorkflowExecutionRequest) (*workflowservice.UpdateWorkflowExecutionResponse, error) {
				return &workflowservice.UpdateWorkflowExecutionResponse{UpdateRef: updateRef(r), Stage: enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED, Outcome: completedMessageOutcome(`{"approved":true}`)}, nil
			}
			retry, e := service.Submit(t.Context(), request)
			require.NoError(t, e)
			require.False(t, retry.Created)
			require.Equal(t, first.Operation.ID, retry.Operation.ID)
			require.Equal(t, first.Operation.ExternalUpdateID, retry.Operation.ExternalUpdateID)
			require.Equal(t, connector.OperationCompleted, retry.Operation.State)
			require.Nil(t, retry.Operation.OpenKey)
			require.False(t, retry.Operation.RetryableUnsent)
			require.Equal(t, 1, raw.sends)
			require.Zero(t, raw.polls)
		})
	}
	// A nil messenger gives no classification code. It still proves this initial
	// call sent nothing; repairing the injected adapter permits the same-key retry.
	raw := &fakeMessageService{}
	service, provider, request := operationBridgeService(t, raw, 0, "")
	messenger := provider.Messenger
	provider.Messenger = nil
	first, e := service.Submit(t.Context(), request)
	require.ErrorIs(t, e, coperations.ErrOperationProvider)
	require.Equal(t, connector.OperationSubmitted, first.Operation.State)
	require.True(t, first.Operation.RetryableUnsent)
	provider.Messenger = messenger
	raw.submit = func(_ context.Context, r *workflowservice.UpdateWorkflowExecutionRequest) (*workflowservice.UpdateWorkflowExecutionResponse, error) {
		return &workflowservice.UpdateWorkflowExecutionResponse{UpdateRef: updateRef(r), Stage: enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED, Outcome: completedMessageOutcome(`{"approved":true}`)}, nil
	}
	retry, e := service.Submit(t.Context(), request)
	require.NoError(t, e)
	require.Equal(t, connector.OperationCompleted, retry.Operation.State)
	require.Equal(t, 1, raw.sends)
}

func TestOperationServiceBridgeDefinitivePreflightIsTerminal(t *testing.T) {
	for _, code := range []codes.Code{codes.NotFound, codes.InvalidArgument, codes.FailedPrecondition, codes.PermissionDenied} {
		t.Run(code.String(), func(t *testing.T) {
			raw := &fakeMessageService{describe: func(context.Context, *workflowservice.DescribeWorkflowExecutionRequest) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
				return nil, status.Error(code, "definitive refusal")
			}}
			service, _, request := operationBridgeService(t, raw, 0, "")
			first, e := service.Submit(t.Context(), request)
			require.ErrorIs(t, e, coperations.ErrOperationProvider)
			require.Equal(t, connector.OperationNotDispatched, first.Operation.State)
			require.False(t, first.Operation.RetryableUnsent)
			require.Nil(t, first.Operation.OpenKey)
			_, e = service.Submit(t.Context(), request)
			require.NoError(t, e)
			require.Zero(t, raw.sends)
			require.Zero(t, raw.polls)
			require.Equal(t, 1, raw.reads)
		})
	}
}

func TestOperationServiceBridgeUnknownNilReplayLooksUpAndReturnsPending(t *testing.T) {
	raw := &fakeMessageService{submit: func(_ context.Context, r *workflowservice.UpdateWorkflowExecutionRequest) (*workflowservice.UpdateWorkflowExecutionResponse, error) {
		return &workflowservice.UpdateWorkflowExecutionResponse{UpdateRef: updateRef(r), Stage: enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_UNSPECIFIED}, nil
	}, poll: func(_ context.Context, r *workflowservice.PollWorkflowExecutionUpdateRequest) (*workflowservice.PollWorkflowExecutionUpdateResponse, error) {
		return &workflowservice.PollWorkflowExecutionUpdateResponse{UpdateRef: r.UpdateRef, Stage: enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_UNSPECIFIED}, nil
	}}
	service, _, request := operationBridgeService(t, raw, 0, "")
	first, e := service.Submit(t.Context(), request)
	require.NoError(t, e)
	require.Equal(t, connector.OperationUnknown, first.Operation.State)
	replay, e := service.Submit(t.Context(), request)
	require.ErrorIs(t, e, coperations.ErrOperationPending)
	require.Equal(t, first.Operation.ID, replay.Operation.ID)
	require.Equal(t, connector.OperationUnknown, replay.Operation.State)
	require.NotNil(t, replay.Operation.OpenKey)
	require.False(t, replay.Operation.RetryableUnsent)
	require.Equal(t, 1, raw.sends)
	require.Equal(t, 1, raw.polls)
}

func TestOperationServiceBridgeExactMaximumResultCompletesWithBoundedOmission(t *testing.T) {
	const maximum = 4096
	encoded, e := json.Marshal(map[string]any{"blob": ""})
	require.NoError(t, e)
	result, e := json.Marshal(map[string]any{"blob": strings.Repeat("x", maximum-len(encoded))})
	require.NoError(t, e)
	require.Len(t, result, maximum)
	raw := &fakeMessageService{submit: func(_ context.Context, r *workflowservice.UpdateWorkflowExecutionRequest) (*workflowservice.UpdateWorkflowExecutionResponse, error) {
		return &workflowservice.UpdateWorkflowExecutionResponse{UpdateRef: updateRef(r), Stage: enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED, Outcome: completedMessageOutcome(string(result))}, nil
	}}
	service, _, request := operationBridgeService(t, raw, maximum, `{"type":"object","properties":{"blob":{"type":"string"}},"required":["blob"],"additionalProperties":false}`)
	receipt, e := service.Submit(t.Context(), request)
	require.NoError(t, e)
	require.Equal(t, connector.OperationCompleted, receipt.Operation.State)
	require.Nil(t, receipt.Operation.OpenKey)
	require.LessOrEqual(t, len(receipt.Operation.ProviderEvidence), maximum)
	var evidence coperations.OperationEvidence
	require.NoError(t, json.Unmarshal(receipt.Operation.ProviderEvidence, &evidence))
	require.True(t, evidence.ResultOmitted)
	require.Empty(t, evidence.Result)
	replay, e := service.Submit(t.Context(), request)
	require.NoError(t, e)
	require.Equal(t, connector.OperationCompleted, replay.Operation.State)
	require.Equal(t, 1, raw.sends)
	require.Zero(t, raw.polls)
}
