package temporal

import (
	"context"
	"errors"

	coperations "github.com/caesium-cloud/caesium/internal/connector/operations"
)

// OperationProvider bridges durable neutral operations to the bounded Messenger.
// The messenger freezes the same configuration as the composing service.
type OperationProvider struct{ Messenger *Messenger }

var _ coperations.OperationProvider = (*OperationProvider)(nil)

func (p *OperationProvider) StableOperationID(id string) (string, error) { return StableUpdateID(id) }

func operationTarget(dispatch coperations.OperationDispatch) (MessageTarget, error) {
	coordinates := dispatch.Target.Execution.Coordinates
	if len(coordinates) != 2 || coordinates["workflow_id"] == "" || coordinates["run_id"] == "" {
		return MessageTarget{}, errors.New("temporal operation requires pinned workflow and run coordinates")
	}
	return MessageTarget{ConnectionID: dispatch.Target.Execution.ConnectionID, WorkflowID: coordinates["workflow_id"], RunID: coordinates["run_id"], BindingName: dispatch.Target.BindingName, BindingVersion: dispatch.Target.BindingVersion}, nil
}

func operationEvidence(result UpdateResult) coperations.OperationEvidence {
	return coperations.OperationEvidence{State: result.State, NativeStage: result.NativeStage, PreflightRPCCode: result.PreflightRPCCode, RPCCode: result.RPCCode, Result: result.Result, FailureKind: result.FailureKind}
}

func (p *OperationProvider) SubmitOperation(ctx context.Context, dispatch coperations.OperationDispatch) (coperations.OperationEvidence, error) {
	if p == nil || p.Messenger == nil {
		return coperations.OperationEvidence{State: UpdateNotDispatched}, errors.New("temporal messenger unavailable")
	}
	target, err := operationTarget(dispatch)
	if err != nil {
		return coperations.OperationEvidence{State: UpdateNotDispatched}, err
	}
	result, err := p.Messenger.SubmitAction(ctx, target, dispatch.Action, dispatch.ExternalUpdateID, dispatch.Input, dispatch.Actor)
	return operationEvidence(result), err
}

func (p *OperationProvider) LookupOperation(ctx context.Context, dispatch coperations.OperationDispatch) (coperations.OperationEvidence, error) {
	if p == nil || p.Messenger == nil {
		return coperations.OperationEvidence{State: UpdateNotDispatched}, errors.New("temporal messenger unavailable")
	}
	target, err := operationTarget(dispatch)
	if err != nil {
		return coperations.OperationEvidence{State: UpdateNotDispatched}, err
	}
	result, err := p.Messenger.LookupAction(ctx, target, dispatch.Action, dispatch.ExternalUpdateID)
	return operationEvidence(result), err
}
