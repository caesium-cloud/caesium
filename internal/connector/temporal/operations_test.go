package temporal

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/connector"
	coperations "github.com/caesium-cloud/caesium/internal/connector/operations"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
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
