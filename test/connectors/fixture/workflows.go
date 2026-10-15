//go:build integration

package main

import (
	"fmt"
	"time"

	"github.com/caesium-cloud/caesium/internal/connector"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const taskQueue = "caesium-connectors-fixture"

type publicationInput struct {
	FirstJob     string            `json:"first_job"`
	SecondJob    string            `json:"second_job"`
	Eligible     bool              `json:"eligible"`
	RetryFirst   bool              `json:"retry_first"`
	MessageOnly  bool              `json:"message_only"`
	MetadataOnly bool              `json:"metadata_only"`
	Child        bool              `json:"child"`
	Resume       *publicationState `json:"resume,omitempty"`
}
type publicationState struct {
	Stage           string                         `json:"stage"`
	Eligible        bool                           `json:"eligible"`
	Approved        bool                           `json:"approved"`
	ApprovalEffects int                            `json:"approval_effects"`
	Actor           *connector.ActorEnvelope       `json:"actor,omitempty"`
	First           *connector.CorrelationEnvelope `json:"first,omitempty"`
	Second          *connector.CorrelationEnvelope `json:"second,omitempty"`
	ChildRunID      string                         `json:"child_run_id,omitempty"`
}
type approvalEnvelope struct {
	Input struct {
		Note string `json:"note"`
	} `json:"input"`
	Actor connector.ActorEnvelope `json:"_caesium_actor"`
}
type approvalResult struct {
	Approved bool `json:"approved"`
}

// Publication stays open after publication so different Update IDs can prove
// the logical effect guard. Only fixture controls can finish/continue it.
func Publication(ctx workflow.Context, in publicationInput) error {
	state := publicationState{Stage: "starting", Eligible: in.Eligible}
	if in.Resume != nil {
		state = *in.Resume
	}
	if err := workflow.SetQueryHandler(ctx, "publication_status", func() (publicationState, error) { return state, nil }); err != nil {
		return err
	}
	if err := workflow.SetQueryHandler(ctx, "fixture_opaque_status", func() (opaqueHint, error) { return opaqueHint{}, nil }); err != nil {
		return err
	}
	if err := workflow.SetUpdateHandlerWithOptions(ctx, "approve_publication", func(_ workflow.Context, envelope approvalEnvelope) (approvalResult, error) {
		if !state.Approved {
			// Set the guard before yielding: concurrent handlers and distinct Update
			// IDs must not schedule another effect. Replay restores the same guard.
			state.Approved = true
			state.ApprovalEffects++
			actor := envelope.Actor
			state.Actor = &actor
		}
		return approvalResult{Approved: true}, nil
	}, workflow.UpdateHandlerOptions{Validator: func(envelope approvalEnvelope) error {
		if err := envelope.Actor.Validate(); err != nil {
			return err
		}
		if envelope.Actor.BindingVersion != "1" || (envelope.Actor.Role != "operator" && envelope.Actor.Role != "admin") {
			return fmt.Errorf("untrusted approval context")
		}
		if !state.Eligible && !state.Approved {
			return fmt.Errorf("publication is not eligible")
		}
		if state.Stage == "starting" {
			return fmt.Errorf("publication data is not ready")
		}
		return nil
	}}); err != nil {
		return err
	}
	workflow.Go(ctx, func(ctx workflow.Context) {
		ch := workflow.GetSignalChannel(ctx, "fixture_eligibility")
		for {
			var eligible bool
			ch.Receive(ctx, &eligible)
			state.Eligible = eligible
		}
	})
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute, HeartbeatTimeout: 5 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, MaximumInterval: time.Second, MaximumAttempts: 5},
	})
	if in.Resume == nil {
		if !in.MessageOnly {
			var first connector.CorrelationEnvelope
			var result any = &first
			if in.MetadataOnly {
				result = nil
			}
			if err := workflow.ExecuteActivity(ctx, "caesium.start", runJobInput{JobID: in.FirstJob, RetryAfterStart: in.RetryFirst, MetadataOnly: in.MetadataOnly}).Get(ctx, result); err != nil {
				return err
			}
			if !in.MetadataOnly {
				state.First = &first
			}
			if in.Child {
				childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{WorkflowID: workflow.GetInfo(ctx).WorkflowExecution.ID + "/child"})
				child := workflow.ExecuteChildWorkflow(childCtx, ExternalChild)
				var execution workflow.Execution
				if err := child.GetChildWorkflowExecution().Get(ctx, &execution); err != nil {
					return err
				}
				state.ChildRunID = execution.RunID
				if err := child.Get(ctx, nil); err != nil {
					return err
				}
			}
		}
		state.Stage = "awaiting_approval"
		if err := workflow.Await(ctx, func() bool { return state.Approved }); err != nil {
			return err
		}
		if err := workflow.ExecuteActivity(ctx, "fixture.effect", *state.Actor).Get(ctx, nil); err != nil {
			return err
		}
		if !in.MessageOnly {
			var second connector.CorrelationEnvelope
			if err := workflow.ExecuteActivity(ctx, "caesium.start", runJobInput{JobID: in.SecondJob}).Get(ctx, &second); err != nil {
				return err
			}
			state.Second = &second
		}
		state.Stage = "published"
	}
	selector := workflow.NewSelector(ctx)
	continued := false
	selector.AddReceive(workflow.GetSignalChannel(ctx, "fixture_continue"), func(c workflow.ReceiveChannel, _ bool) { var v bool; c.Receive(ctx, &v); continued = v })
	selector.AddReceive(workflow.GetSignalChannel(ctx, "fixture_finish"), func(c workflow.ReceiveChannel, _ bool) { var v bool; c.Receive(ctx, &v) })
	selector.Select(ctx)
	if err := workflow.Await(ctx, func() bool { return workflow.AllHandlersFinished(ctx) }); err != nil {
		return err
	}
	if continued {
		in.Resume = &state
		return workflow.NewContinueAsNewError(ctx, Publication, in)
	}
	return nil
}

func ExternalOnly(ctx workflow.Context) error {
	if err := workflow.SetQueryHandler(ctx, "external_status", func() (map[string]string, error) { return map[string]string{"stage": "external_wait"}, nil }); err != nil {
		return err
	}
	var finish bool
	workflow.GetSignalChannel(ctx, "fixture_finish").Receive(ctx, &finish)
	return nil
}
func ExternalChild(workflow.Context) error { return nil }
