package incident

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// approval.go is the two ends of the tier-3 approval pipeline
// (trust-the-substrate C4 + C7):
//
//	proposal → ApprovalRequest → (human decides) → ExecuteApproved → dispatch
//
// Before this, Execute()'s decisionApprove branch recorded a `proposed` row and
// stopped — nothing created the ApprovalRequest a human decides on, and the
// approvals service marked an action `approved` with nothing to execute it. The
// two halves live here so the invariant they jointly enforce is readable in one
// file: a tier-3 action executes if and only if a pending ApprovalRequest for it
// was resolved `approved` by an authenticated human operator.

// ErrActionNotApproved is returned by ExecuteApproved when the action is not in
// the `approved` state — it was never approved, was rejected, or has already
// run. It is the executor-side half of the once-only guarantee whose other half
// is the approvals service's conditional `decision = pending` update.
var ErrActionNotApproved = errors.New("incident: action is not approved")

// approvalExpiry bounds how long a tier-3 proposal waits for a human before the
// request is stale. It is recorded on the row for the operator surfaces; nothing
// auto-expires today (a sweeper is a later refinement), so it is advisory.
const approvalExpiry = 24 * time.Hour

// requestApproval creates the ApprovalRequest for a tier-3 proposal, parks the
// incident in awaiting_approval, and announces it on the event stream.
//
// The status move goes through the store's transition table rather than a bare
// UPDATE, so an incident that a human already took over (escalated, closed) is
// not yanked back into awaiting_approval by a late agent proposal. An incident
// still in `open` is walked open → triaging → awaiting_approval, because the
// status machine has no open → awaiting_approval edge and a proposal is by
// definition triage having happened.
func (e *Executor) requestApproval(ctx context.Context, inc *models.Incident, action *models.AgentAction) (*models.ApprovalRequest, error) {
	now := time.Now().UTC()
	expires := now.Add(approvalExpiry)
	approval := &models.ApprovalRequest{
		ID:         uuid.New(),
		Namespace:  inc.Namespace,
		IncidentID: inc.ID,
		ActionID:   action.ID,
		Decision:   models.ApprovalDecisionPending,
		ExpiresAt:  &expires,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := e.store.DB().WithContext(ctx).Create(approval).Error; err != nil {
		return nil, fmt.Errorf("incident: create approval request: %w", err)
	}

	e.parkAwaitingApproval(ctx, inc)
	e.publishEvent(ctx, event.Event{
		Type:      event.TypeApprovalRequested,
		JobID:     inc.JobID,
		Timestamp: now,
		Payload: encodeEventPayload(map[string]any{
			"incident_id": inc.ID.String(),
			"approval_id": approval.ID.String(),
			"action_id":   action.ID.String(),
			"action_type": action.Type,
			"tier":        action.Tier,
		}),
	})
	return approval, nil
}

// parkAwaitingApproval advances the incident to awaiting_approval, walking the
// open → triaging edge first when needed. Best-effort by design: a failure to
// park must not lose the approval row (the human decision surface), so it is
// logged rather than returned. An incident already in awaiting_approval, or past
// it, is left alone.
func (e *Executor) parkAwaitingApproval(ctx context.Context, inc *models.Incident) {
	if inc.Status == models.IncidentStatusAwaitingApproval {
		return
	}
	if inc.Status == models.IncidentStatusOpen {
		moved, err := e.store.Transition(ctx, inc.ID, models.IncidentStatusTriaging, "")
		if err != nil {
			log.Warn("incident: could not move incident to triaging for approval",
				"incident_id", inc.ID, "from", inc.Status, "error", err)
			return
		}
		inc.Status = moved.Status
	}
	moved, err := e.store.Transition(ctx, inc.ID, models.IncidentStatusAwaitingApproval, "")
	if err != nil {
		log.Warn("incident: could not park incident awaiting approval",
			"incident_id", inc.ID, "from", inc.Status, "error", err)
		return
	}
	inc.Status = moved.Status
}

// endSessionForApproval ends the proposing agent session while the approval is
// pending: design-agent-in-the-loop.md's "the agent session ends (no idle
// container burning tokens); a fresh session resumes on decision if follow-up
// work is needed". Best-effort — the approval is already durable, and a session
// row that outlives its container is a cosmetic defect, not a correctness one.
func (e *Executor) endSessionForApproval(ctx context.Context, sessionID *uuid.UUID, inc *models.Incident, approval *models.ApprovalRequest) {
	if sessionID == nil || *sessionID == uuid.Nil {
		return
	}
	sup := SessionSupervisor()
	if sup == nil {
		return
	}
	if err := sup.EndSession(ctx, *sessionID); err != nil {
		log.Warn("incident: could not end agent session for pending approval",
			"incident_id", inc.ID, "session_id", *sessionID, "approval_id", approval.ID, "error", err)
	}
}

// ExecuteApproved dispatches a tier-3 action a human approved.
//
// It is invoked by the incident REST service AFTER the approval decision
// transaction commits (api/rest/service/incident/approvals.go), which is what
// makes the audit spine honest: the row moves proposed → approved → executed,
// each step durable before the next begins, and a crash between them leaves an
// approved-but-unexecuted action a human can see rather than a silent nothing.
//
// Guards:
//   - the action must be in `approved` state, so a rejected action, an
//     already-executed one, and a raw proposal are all refused;
//   - the incident boundary is re-verified inside dispatch, so an approval
//     cannot be used to reach a run or job outside the incident's own;
//   - the decider is read from the ApprovalRequest, never from the caller, so
//     the audit actor is the recorded human decision.
func (e *Executor) ExecuteApproved(ctx context.Context, actionID uuid.UUID) (*models.AgentAction, error) {
	var action models.AgentAction
	if err := e.store.DB().WithContext(ctx).First(&action, "id = ?", actionID).Error; err != nil {
		return nil, fmt.Errorf("incident: load approved action: %w", err)
	}
	if action.Status != models.AgentActionStatusApproved {
		return &action, fmt.Errorf("%w: action %s is %s", ErrActionNotApproved, actionID, action.Status)
	}

	inc, err := e.store.Get(ctx, action.IncidentID)
	if err != nil {
		return &action, fmt.Errorf("incident: load incident for approved action: %w", err)
	}
	if inc == nil {
		return &action, errors.New("incident: incident not found for approved action")
	}

	var params ActionParams
	if len(action.Params) > 0 {
		if err := json.Unmarshal(action.Params, &params); err != nil {
			e.finishAs(ctx, &action, models.AgentActionStatusFailed,
				map[string]any{"error": "decode action params: " + err.Error()}, "")
			return &action, fmt.Errorf("incident: decode approved action params: %w", err)
		}
	}

	decider, decidedAt := e.approvalDecision(ctx, action.ID)
	auditActor := "human:" + decider

	// The playbook is not re-consulted: a human decision supersedes it. The
	// allowlist governs what the AGENT may do autonomously; an operator who
	// approved this exact action has already answered that question.
	result, execErr := e.dispatch(ctx, action.Type, inc, &action, params, Playbook{})
	if execErr != nil {
		e.finishAs(ctx, &action, models.AgentActionStatusFailed, map[string]any{
			"error":       execErr.Error(),
			"approved_by": decider,
		}, auditActor)
		e.publishActionExecuted(ctx, inc, &action, decider, decidedAt, false)
		return &action, execErr
	}

	if result == nil {
		result = map[string]any{}
	}
	result["approved_by"] = decider
	if decidedAt != nil {
		result["approved_at"] = decidedAt.UTC().Format(time.RFC3339)
	}
	result["executed_at"] = time.Now().UTC().Format(time.RFC3339)
	e.finishAs(ctx, &action, models.AgentActionStatusExecuted, result, auditActor)
	e.publishActionExecuted(ctx, inc, &action, decider, decidedAt, true)
	return &action, nil
}

// approvalDecision returns the recorded decider and decision time for an action.
// A missing approval row degrades to the generic "operator" label rather than
// failing the execution — the decision that authorised this call already
// committed; losing its label must not undo it.
func (e *Executor) approvalDecision(ctx context.Context, actionID uuid.UUID) (string, *time.Time) {
	var approval models.ApprovalRequest
	err := e.store.DB().WithContext(ctx).
		Where("action_id = ?", actionID).
		Order("created_at DESC").
		First(&approval).Error
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			log.Warn("incident: could not read approval for executed action", "action_id", actionID, "error", err)
		}
		return "operator", nil
	}
	decider := approval.Decider
	if decider == "" {
		decider = "operator"
	}
	return decider, approval.DecidedAt
}

// publishActionExecuted emits the explainability event for an approved action
// that ran (trust-the-substrate C8). It carries the decider, the action type and
// tier, and the outcome, so "approved by <decider>, executed at <ts>" is
// reconstructable from the event stream alone — and so a notification policy can
// route it like any other operational event.
func (e *Executor) publishActionExecuted(ctx context.Context, inc *models.Incident, action *models.AgentAction, decider string, decidedAt *time.Time, ok bool) {
	payload := map[string]any{
		"incident_id": inc.ID.String(),
		"action_id":   action.ID.String(),
		"action_type": action.Type,
		"tier":        action.Tier,
		"status":      string(action.Status),
		"approved_by": decider,
		"succeeded":   ok,
		"job_id":      inc.JobID.String(),
	}
	if decidedAt != nil {
		payload["approved_at"] = decidedAt.UTC().Format(time.RFC3339)
	}
	if inc.TaskName != "" {
		payload["task_name"] = inc.TaskName
	}
	evt := event.Event{
		Type:      event.TypeAgentActionExecuted,
		JobID:     inc.JobID,
		Timestamp: time.Now().UTC(),
		Payload:   encodeEventPayload(payload),
	}
	if inc.RemediationTargetRunID != nil {
		evt.RunID = *inc.RemediationTargetRunID
	} else if inc.RunID != nil {
		evt.RunID = *inc.RunID
	}
	e.publishEvent(ctx, evt)
}

// encodeEventPayload marshals an event payload, returning nil on error so a
// payload defect degrades the event's detail rather than losing the event.
func encodeEventPayload(v map[string]any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return json.RawMessage(b)
}

// publishEvent persists (when an event store is wired) and publishes an event,
// mirroring notification.Watcher.persistAndPublish. Persist-then-publish is the
// order that makes the event queryable from /v1/events even if no live
// subscriber was listening at the moment it fired.
func (e *Executor) publishEvent(ctx context.Context, evt event.Event) {
	if e.bus == nil && e.eventStore == nil {
		return
	}
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now().UTC()
	}
	if e.eventStore != nil {
		if err := e.store.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return e.eventStore.AppendTx(tx, &evt)
		}); err != nil {
			log.Warn("incident: failed to persist event", "event_type", string(evt.Type), "error", err)
			return
		}
	}
	event.PublishAndMarkBusDispatched(ctx, e.bus, e.eventStore, evt)
}
