package incident

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
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

	// bus and eventStore carry the approval/execution lifecycle onto the shared
	// event stream. Both are optional (nil in unit tests); when eventStore is set
	// the event is PERSISTED first and then published, mirroring the notification
	// watcher's persistAndPublish, so `approval_requested` /
	// `agent_action_executed` survive a restart and are queryable from
	// /v1/events rather than existing only as an in-memory fan-out.
	bus        event.Bus
	eventStore *event.Store
}

// SetEventSink wires the process event bus and the durable event store onto the
// executor. Wired once at startup (cmd/start/start.go) behind the remediation
// master gate; nil-safe, so a test executor simply emits nothing.
func (e *Executor) SetEventSink(bus event.Bus, store *event.Store) {
	e.bus = bus
	e.eventStore = store
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

// ErrRetryDeferred marks a retry refused by a transient, retryable admission
// condition — the job is paused, or a concurrency slot is not yet free — that
// will clear on its own. ActionOps.RetryFromFailure implementations return it
// (wrapping the underlying cause) so a fired snooze_retry timer re-arms instead
// of being consumed and lost. A non-ErrRetryDeferred error is treated as a
// permanent failure.
var ErrRetryDeferred = errors.New("incident: retry deferred; retry once the condition clears")

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

// playbookDocument mirrors the JSON shape stored on AgentProfile.Playbook (see
// agentprofile.SeedDefaults) and the `metadata.remediation` block in
// pkg/jobdef.RemediationAutonomy, so one decoder serves both: the profile-level
// document and the job-level block now persisted on models.Job.Remediation.
type playbookDocument struct {
	Autonomy struct {
		Allow           []string            `json:"allow"`
		ParamOverrides  map[string][]string `json:"paramOverrides"`
		RequireApproval []string            `json:"requireApproval"`
	} `json:"autonomy"`
}

// DecodePlaybook parses a stored playbook document into the enforcement input.
// An empty or malformed document yields the zero Playbook (tier 3 → approval),
// never a permissive one.
func DecodePlaybook(raw []byte) Playbook {
	if len(raw) == 0 {
		return Playbook{}
	}
	var doc playbookDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		log.Warn("incident: could not decode playbook; falling back to the default policy", "error", err)
		return Playbook{}
	}
	pb := Playbook{ParamOverrides: doc.Autonomy.ParamOverrides}
	if len(doc.Autonomy.Allow) > 0 {
		pb.Allow = make(map[string]bool, len(doc.Autonomy.Allow))
		for _, a := range doc.Autonomy.Allow {
			pb.Allow[a] = true
		}
	}
	if len(doc.Autonomy.RequireApproval) > 0 {
		pb.RequireApproval = make(map[string]bool, len(doc.Autonomy.RequireApproval))
		for _, a := range doc.Autonomy.RequireApproval {
			pb.RequireApproval[a] = true
		}
	}
	return pb
}

// Narrow combines two playbooks so the RESULT IS NEVER WIDER THAN EITHER — the
// only safe direction when a job-level policy meets its profile's. It is how a
// job's `metadata.remediation.autonomy` block constrains the AgentProfile
// playbook it names: neither document can grant what the other withholds.
//
// Per field, following the enforcement semantics in decide/allowsAutonomous:
//   - Allow: an EMPTY set means "unconstrained" (tier 0/1 default autonomous),
//     so narrowing empty with a list yields the list, and two lists intersect.
//   - RequireApproval: a union — either side may force the approval gate.
//   - ParamOverrides: a missing key is denied, so keys intersect; per key an
//     empty value list means "any value", so it yields to the other side's list
//     and two lists intersect.
func (pb Playbook) Narrow(other Playbook) Playbook {
	out := Playbook{}

	switch {
	case len(pb.Allow) == 0:
		out.Allow = other.Allow
	case len(other.Allow) == 0:
		out.Allow = pb.Allow
	default:
		out.Allow = make(map[string]bool)
		for action := range pb.Allow {
			if pb.Allow[action] && other.Allow[action] {
				out.Allow[action] = true
			}
		}
		// An intersection that empties out must NOT read as "unconstrained": two
		// disjoint allowlists agree on nothing, so keep a sentinel that allows no
		// action rather than collapsing to the permissive empty set.
		if len(out.Allow) == 0 {
			out.Allow = map[string]bool{allowNothingSentinel: false}
		}
	}

	if len(pb.RequireApproval) > 0 || len(other.RequireApproval) > 0 {
		out.RequireApproval = make(map[string]bool, len(pb.RequireApproval)+len(other.RequireApproval))
		for action, required := range pb.RequireApproval {
			if required {
				out.RequireApproval[action] = true
			}
		}
		for action, required := range other.RequireApproval {
			if required {
				out.RequireApproval[action] = true
			}
		}
	}

	switch {
	case len(pb.ParamOverrides) == 0:
		out.ParamOverrides = other.ParamOverrides
	case len(other.ParamOverrides) == 0:
		out.ParamOverrides = pb.ParamOverrides
	default:
		out.ParamOverrides = make(map[string][]string)
		for key, mine := range pb.ParamOverrides {
			theirs, ok := other.ParamOverrides[key]
			if !ok {
				continue
			}
			out.ParamOverrides[key] = intersectValues(mine, theirs)
		}
	}

	return out
}

// allowNothingSentinel is an action type no catalog entry can ever use, so an
// Allow map containing only it is non-empty (hence "configured") while matching
// nothing. It makes "these two policies allow nothing in common" expressible in
// a map whose emptiness already means the opposite.
const allowNothingSentinel = "\x00none"

// DenyAllPlaybook is the fail-closed policy: no action type is autonomously
// permitted at any tier, so every proposal is either denied or routed to a
// human. It is what a caller uses when a job's DECLARED policy cannot be
// resolved — substituting any other policy there would enforce something the
// job did not ask for, in the widening direction.
//
// It is deliberately distinct from the zero Playbook, which means "unconfigured"
// and still lets tier 0/1 run autonomously.
func DenyAllPlaybook() Playbook {
	return Playbook{Allow: map[string]bool{allowNothingSentinel: false}}
}

// intersectValues intersects two rerun_with_params value whitelists, treating an
// empty list as "any value" (so it yields to the other side).
func intersectValues(a, b []string) []string {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]struct{}, len(b))
	for _, v := range b {
		seen[v] = struct{}{}
	}
	out := make([]string, 0, len(a))
	for _, v := range a {
		if _, ok := seen[v]; ok {
			out = append(out, v)
		}
	}
	// An empty intersection must deny every value, not admit every value, so
	// return a list that matches nothing rather than the "any value" empty list.
	if len(out) == 0 {
		return []string{allowNothingSentinel}
	}
	return out
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
	if inc == nil {
		return nil, errors.New("incident: incident not found")
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
		// Tier 3 (or a playbook-forced approval): record the proposal, create the
		// ApprovalRequest a human decides on, park the incident in
		// awaiting_approval, and end the agent session so no container idles while
		// a human thinks. NOTHING executes here — ExecuteApproved is the only path
		// that dispatches a tier-3 action, and only after a recorded decision.
		e.observe(action)
		e.mirrorAudit(ctx, action, "proposed", "")
		approval, err := e.requestApproval(ctx, inc, action)
		if err != nil {
			// The approval row is the ONLY way a human can act on this proposal, so
			// failing to create it must not look like a successful proposal.
			e.finish(ctx, action, models.AgentActionStatusFailed, map[string]any{"error": err.Error()})
			return action, err
		}
		e.endSessionForApproval(ctx, req.SessionID, inc, approval)
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
	if inc == nil {
		return nil, errors.New("incident: incident not found")
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
	e.finishAs(ctx, action, status, result, "")
}

// finishAs is finish with an explicit audit-log actor. auditActor is empty for
// the autonomous path (the audit row then names the action's own actor) and
// carries "human:<decider>" for an approved execution, so the audit spine
// records WHO approved the mutation, not merely that an agent proposed it.
func (e *Executor) finishAs(ctx context.Context, action *models.AgentAction, status models.AgentActionStatus, result any, auditActor string) {
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
	e.mirrorAudit(ctx, action, string(status), auditActor)
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
func (e *Executor) mirrorAudit(ctx context.Context, action *models.AgentAction, outcome, auditActor string) {
	if action.Tier < TierGated {
		return
	}
	// Policy denials (rejected) are not executions; they live only on the action
	// timeline, not the audit log.
	if action.Status == models.AgentActionStatusRejected {
		return
	}
	actor := "agent:" + string(action.Actor)
	if auditActor != "" {
		actor = auditActor
	}
	entry := &models.AuditLog{
		ID:           uuid.New(),
		Timestamp:    time.Now().UTC(),
		Actor:        actor,
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
	err := e.ops.RetryFromFailure(ctx, payload.RunID)
	if err == nil {
		return nil
	}
	// The supervisor already claimed this timer as fired. A retryable refusal
	// (job paused, or capacity not yet free) means the retry never happened, so
	// consuming the timer would silently drop the remediation. Re-arm a fresh
	// timer with bounded backoff instead so the retry lands once the condition
	// clears; a permanent failure (e.g. the run is gone) is consumed as before.
	if errors.Is(err, ErrRetryDeferred) {
		return e.rearmSnooze(ctx, timer, payload, err)
	}
	return fmt.Errorf("incident: snooze_retry fire: %w", err)
}

// snooze re-arm bounds: exponential backoff base × 2^rearm, capped, with a hard
// re-arm ceiling so a job left paused forever cannot re-arm indefinitely.
const (
	snoozeRearmBackoffBase = time.Minute
	snoozeRearmBackoffMax  = time.Hour
	maxSnoozeRearm         = 12
)

// snoozeRearmBackoff returns the bounded backoff before the nth re-arm.
func snoozeRearmBackoff(rearm int) time.Duration {
	d := snoozeRearmBackoffBase
	for i := 1; i < rearm && d < snoozeRearmBackoffMax; i++ {
		d *= 2
	}
	if d > snoozeRearmBackoffMax {
		return snoozeRearmBackoffMax
	}
	return d
}

// rearmSnooze schedules a fresh snooze timer after a retryable refusal, recording
// the re-arm on the audit spine. Beyond maxSnoozeRearm it gives up (records a
// failed action and returns the cause so the sweeper logs it), so a permanently
// paused job cannot loop forever.
func (e *Executor) rearmSnooze(ctx context.Context, timer models.RemediationTimer, payload snoozePayload, cause error) error {
	if payload.Rearm >= maxSnoozeRearm {
		e.recordSnoozeEvent(ctx, timer, models.AgentActionStatusFailed, map[string]any{
			"run_id":     payload.RunID.String(),
			"rearm":      payload.Rearm,
			"gave_up":    true,
			"last_error": cause.Error(),
		})
		return fmt.Errorf("incident: snooze_retry exhausted %d re-arms: %w", maxSnoozeRearm, cause)
	}
	next := payload
	next.Rearm++
	fireAt := time.Now().UTC().Add(snoozeRearmBackoff(next.Rearm))
	newTimer, err := e.store.ScheduleTimer(ctx, timer.IncidentID, TimerKindSnoozeRetry, fireAt, encodeJSON(next), timer.ActionID, timer.Namespace)
	if err != nil {
		return fmt.Errorf("incident: re-arm snooze timer: %w", err)
	}
	e.recordSnoozeEvent(ctx, timer, models.AgentActionStatusExecuted, map[string]any{
		"run_id":        payload.RunID.String(),
		"rearm":         next.Rearm,
		"deferred":      true,
		"reason":        cause.Error(),
		"next_timer_id": newTimer.ID.String(),
		"fire_at":       fireAt.Format(time.RFC3339),
	})
	log.Info("incident: snooze_retry deferred; re-armed",
		"incident_id", timer.IncidentID,
		"run_id", payload.RunID,
		"rearm", next.Rearm,
		"fire_at", fireAt,
		"reason", cause,
	)
	return nil
}

// recordSnoozeEvent appends a policy AgentAction row documenting a snooze re-arm
// or give-up, so the deferral is observable on the incident timeline.
func (e *Executor) recordSnoozeEvent(ctx context.Context, timer models.RemediationTimer, status models.AgentActionStatus, result map[string]any) {
	now := time.Now().UTC()
	action := &models.AgentAction{
		ID:         uuid.New(),
		Namespace:  timer.Namespace,
		IncidentID: timer.IncidentID,
		Type:       ActionTypeSnoozeRetry,
		Tier:       TierAutonomous,
		Status:     status,
		Actor:      models.AgentActionActorPolicy,
		Result:     encodeJSON(result),
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := e.store.DB().WithContext(ctx).Create(action).Error; err != nil {
		log.Warn("incident: failed to record snooze re-arm event", "incident_id", timer.IncidentID, "error", err)
		return
	}
	e.observe(action)
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
