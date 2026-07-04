package incident

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// Action tiers (design-agent-in-the-loop.md, "Action catalog: typed,
// server-enforced, tiered"). Tier semantics: tier 0/1 default autonomous, tier 2
// autonomous only if explicitly allowed by the playbook, tier 3 always produces
// an ApprovalRequest and is never auto-executed in v1 regardless of config.
const (
	TierReadOnly   = 0
	TierAutonomous = 1
	TierGated      = 2
	TierApproval   = 3
)

// Executor is the typed, server-enforced action layer. The agent never gets
// shell, SQL, or generic HTTP: every mutation arrives as a typed action, is
// validated against the effective playbook, executed server-side through the
// injected ActionOps, and recorded as an AgentAction audit row with the right
// actor/tier/status. Deterministic Phase-0 rules run through the same recording
// path with actor=policy and no container launch.
type Executor struct {
	store *Store
	ops   ActionOps
}

// NewExecutor constructs an executor over the incident store and the action
// operations surface (implemented by Stream C's concrete adapters; a fake in
// tests). A nil ops is tolerated for actions that never dispatch through it
// (deny/approve paths), but executing a dispatching action then panics — callers
// must supply ops in any path that executes.
func NewExecutor(store *Store, ops ActionOps) *Executor {
	return &Executor{store: store, ops: ops}
}

// ErrUnknownAction is returned when the action type is not in the catalog.
var ErrUnknownAction = errors.New("incident: unknown action type")

// ErrActionNotPermitted is returned when the effective playbook denies an
// action (not autonomously allowed and not routed to approval).
var ErrActionNotPermitted = errors.New("incident: action not permitted by playbook")

// Playbook is the effective, resolved remediation policy the executor enforces
// for one incident. Stream E produces it from metadata.remediation overriding
// the AgentProfile defaults; here it is purely the enforcement input. A zero
// Playbook (no Allow/RequireApproval) means "unconfigured": tier 0/1 actions
// default autonomous, tier 2 requires explicit allow, tier 3 always approval.
type Playbook struct {
	// Allow is the set of action types the agent may take autonomously.
	Allow map[string]bool
	// RequireApproval forces listed action types through the approval gate even
	// if their tier would otherwise be autonomous.
	RequireApproval map[string]bool
	// ParamOverrides whitelists rerun_with_params keys → allowed values.
	ParamOverrides map[string][]string
}

// decision is the executor's routing verdict for one action.
type decision int

const (
	decisionExecute decision = iota
	decisionApprove
	decisionDeny
)

// allowsAutonomous reports whether an action type may run autonomously under the
// playbook's allow list. An empty allow list means unconfigured — tier 0/1
// default autonomous — so it returns true; a non-empty list is the
// server-enforced allowlist and governs.
func (pb Playbook) allowsAutonomous(actionType string) bool {
	if len(pb.Allow) == 0 {
		return true
	}
	return pb.Allow[actionType]
}

// decide routes an action to execute / approve / deny per the tier semantics.
func (pb Playbook) decide(actionType string, tier int) decision {
	// An explicit approval requirement always wins for non-fatal tiers.
	if pb.RequireApproval[actionType] {
		return decisionApprove
	}
	// Tier 3 always terminates at a human, never auto-executed in v1.
	if tier >= TierApproval {
		return decisionApprove
	}
	if tier <= TierAutonomous {
		// Tier 0/1 default autonomous, still subject to the allowlist if present.
		if pb.allowsAutonomous(actionType) {
			return decisionExecute
		}
		return decisionDeny
	}
	// Tier 2: autonomous only if explicitly allowed.
	if pb.Allow[actionType] {
		return decisionExecute
	}
	return decisionDeny
}

// ActionRequest is one typed action to validate, record, and (when permitted)
// execute against an incident.
type ActionRequest struct {
	IncidentID uuid.UUID
	// SessionID links the action to an agent session (nil for actor=policy|human).
	SessionID *uuid.UUID
	// Actor originates the row (agent|human; deterministic rules use ExecutePolicy
	// which stamps policy).
	Actor models.AgentActionActor
	// Type is the catalog action type (e.g. "retry_from_failure").
	Type string
	// Params carries the typed action parameters.
	Params ActionParams
	// Playbook is the effective policy the action is validated against.
	Playbook Playbook
}

// Execute validates a typed action against the effective playbook, records it as
// an AgentAction row, and — when the playbook permits autonomous execution —
// dispatches it server-side. Tier-3 actions and playbook-gated actions are
// recorded as proposed (awaiting approval) without executing; playbook-denied
// actions are recorded as rejected and return ErrActionNotPermitted.
func (e *Executor) Execute(ctx context.Context, req ActionRequest) (*models.AgentAction, error) {
	tier, ok := ActionTier(req.Type)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownAction, req.Type)
	}
	actor := req.Actor
	if actor == "" {
		actor = models.AgentActionActorAgent
	}

	inc, err := e.store.Get(ctx, req.IncidentID)
	if err != nil {
		return nil, fmt.Errorf("incident: load incident for action: %w", err)
	}

	dec := req.Playbook.decide(req.Type, tier)

	action := e.newAction(inc, req, tier, actor)
	if err := e.store.DB().WithContext(ctx).Create(action).Error; err != nil {
		return nil, fmt.Errorf("incident: record action: %w", err)
	}

	switch dec {
	case decisionDeny:
		e.finish(ctx, action, models.AgentActionStatusRejected, map[string]any{
			"reason": "action not permitted by effective playbook",
		})
		return action, fmt.Errorf("%w: %s", ErrActionNotPermitted, req.Type)
	case decisionApprove:
		// Recorded as proposed; the tier-3 approval flow (Stream D / B4) creates
		// the ApprovalRequest and resolves it. No execution here.
		e.observe(action)
		e.mirrorAudit(ctx, action, "proposed")
		return action, nil
	}

	// decisionExecute: dispatch server-side.
	result, execErr := e.dispatch(ctx, req.Type, inc, action, req.Params, req.Playbook)
	if execErr != nil {
		e.finish(ctx, action, models.AgentActionStatusFailed, map[string]any{"error": execErr.Error()})
		return action, execErr
	}
	e.finish(ctx, action, models.AgentActionStatusExecuted, result)
	return action, nil
}

// ExecutePolicy runs a deterministic server-side action as actor=policy: no
// playbook allowlist gate (a deterministic rule is pre-approved by being
// deterministic) and no agent session, but the same audit recording, metric, and
// dispatch path. Used by the Phase-0 deterministic rules (rules.go).
func (e *Executor) ExecutePolicy(ctx context.Context, incidentID uuid.UUID, actionType string, params ActionParams) (*models.AgentAction, error) {
	tier, ok := ActionTier(actionType)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownAction, actionType)
	}
	inc, err := e.store.Get(ctx, incidentID)
	if err != nil {
		return nil, fmt.Errorf("incident: load incident for policy action: %w", err)
	}
	action := e.newAction(inc, ActionRequest{
		IncidentID: incidentID,
		Type:       actionType,
		Params:     params,
	}, tier, models.AgentActionActorPolicy)
	if err := e.store.DB().WithContext(ctx).Create(action).Error; err != nil {
		return nil, fmt.Errorf("incident: record policy action: %w", err)
	}
	result, execErr := e.dispatch(ctx, actionType, inc, action, params, Playbook{})
	if execErr != nil {
		e.finish(ctx, action, models.AgentActionStatusFailed, map[string]any{"error": execErr.Error()})
		return action, execErr
	}
	e.finish(ctx, action, models.AgentActionStatusExecuted, result)
	return action, nil
}

// newAction builds a proposed AgentAction row for an incident.
func (e *Executor) newAction(inc *models.Incident, req ActionRequest, tier int, actor models.AgentActionActor) *models.AgentAction {
	now := time.Now().UTC()
	return &models.AgentAction{
		ID:         uuid.New(),
		Namespace:  inc.Namespace,
		IncidentID: inc.ID,
		SessionID:  req.SessionID,
		Type:       req.Type,
		Params:     req.Params.encode(),
		Tier:       tier,
		Status:     models.AgentActionStatusProposed,
		Actor:      actor,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

// finish stamps a terminal status + result on an action, emits the metric, and
// mirrors tier-2/3 executions into the audit log.
func (e *Executor) finish(ctx context.Context, action *models.AgentAction, status models.AgentActionStatus, result any) {
	action.Status = status
	action.Result = encodeJSON(result)
	action.UpdatedAt = time.Now().UTC()
	if err := e.store.DB().WithContext(ctx).
		Model(&models.AgentAction{}).
		Where("id = ?", action.ID).
		Updates(map[string]any{
			"status":     status,
			"result":     action.Result,
			"updated_at": action.UpdatedAt,
		}).Error; err != nil {
		log.Warn("incident: failed to update action status", "action_id", action.ID, "error", err)
	}
	e.observe(action)
	e.mirrorAudit(ctx, action, string(status))
}

// observe increments caesium_agent_actions_total for this action.
func (e *Executor) observe(action *models.AgentAction) {
	metrics.AgentActionsTotal.
		WithLabelValues(action.Type, strconv.Itoa(action.Tier), string(action.Actor)).
		Inc()
}

// mirrorAudit writes tier-2/3 executions into AuditLog (design Security Posture:
// "AuditLog entries mirror tier 2/3 executions"). Tier 0/1 rows live only in the
// AgentAction timeline.
func (e *Executor) mirrorAudit(ctx context.Context, action *models.AgentAction, outcome string) {
	if action.Tier < TierGated {
		return
	}
	// Policy denials (rejected) are not executions; they live only on the action
	// timeline, not the audit log.
	if action.Status == models.AgentActionStatusRejected {
		return
	}
	entry := &models.AuditLog{
		ID:           uuid.New(),
		Timestamp:    time.Now().UTC(),
		Actor:        "agent:" + string(action.Actor),
		Action:       "agent.action." + action.Type,
		ResourceType: "incident",
		ResourceID:   action.IncidentID.String(),
		Outcome:      outcome,
		Metadata:     encodeJSON(map[string]any{"action_id": action.ID.String(), "tier": action.Tier}),
	}
	if err := e.store.DB().WithContext(ctx).Create(entry).Error; err != nil {
		log.Warn("incident: failed to mirror action to audit log", "action_id", action.ID, "error", err)
	}
	metrics.AuditLogEntriesTotal.WithLabelValues(entry.Action, outcome).Inc()
}

// RegisterTimerHandlers wires the durable-timer handlers this executor owns onto
// a TimerSupervisor. snooze_retry timers fire an admit-aware retry when due.
func (e *Executor) RegisterTimerHandlers(sup *TimerSupervisor) {
	sup.RegisterHandler(TimerKindSnoozeRetry, e.fireSnoozeRetry)
}

// fireSnoozeRetry is the durable-timer handler for a due snooze_retry: it runs
// the admit-aware retry for the snoozed run.
func (e *Executor) fireSnoozeRetry(ctx context.Context, timer models.RemediationTimer) error {
	var payload snoozePayload
	if len(timer.Payload) > 0 {
		if err := json.Unmarshal(timer.Payload, &payload); err != nil {
			return fmt.Errorf("incident: decode snooze timer payload: %w", err)
		}
	}
	if payload.RunID == uuid.Nil {
		return errors.New("incident: snooze timer missing run id")
	}
	if e.ops == nil {
		return errors.New("incident: no action ops configured for snooze_retry")
	}
	if err := e.ops.RetryFromFailure(ctx, payload.RunID); err != nil {
		return fmt.Errorf("incident: snooze_retry fire: %w", err)
	}
	return nil
}

// encodeJSON marshals v into datatypes.JSON, returning nil on nil/empty or error.
func encodeJSON(v any) datatypes.JSON {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return datatypes.JSON(b)
}
