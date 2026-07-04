package incident

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// Typed action catalog (design-agent-in-the-loop.md). The agent never gets
// shell, SQL, or generic HTTP; it selects one of these typed actions and the
// executor validates + dispatches it server-side onto machinery that already
// exists.
const (
	// Tier 1 — default autonomous.
	ActionTypeQuarantineReplay = "quarantine_replay"
	ActionTypeSnoozeRetry      = "snooze_retry"
	ActionTypeRetryFromFailure = "retry_from_failure"
	ActionTypeRetryCallbacks   = "retry_callbacks"
	ActionTypeNotify           = "notify"
	ActionTypeEscalate         = "escalate"

	// Tier 2 — autonomous only if explicitly allowed by the playbook.
	ActionTypeRerunWithParams          = "rerun_with_params"
	ActionTypePauseJob                 = "pause_job"
	ActionTypeUnpauseJob               = "unpause_job"
	ActionTypeClearCacheEntry          = "clear_cache_entry"
	ActionTypeSuppressDownstreamAlerts = "suppress_downstream_alerts"
	ActionTypeExtendSLAOnce            = "extend_sla_once"

	// Tier 3 — always approval-gated, never auto-executed. The producers ship in
	// Stream B4; they are registered here so the executor knows their tier and
	// routes them through the approval gate rather than rejecting them as unknown.
	ActionTypeSkipTask           = "skip_task"
	ActionTypeOverrideSchemaGate = "override_schema_gate"
	ActionTypeApplyJobdefPatch   = "apply_jobdef_patch"
)

// TimerKindSnoozeRetry identifies snooze_retry durable timers.
const TimerKindSnoozeRetry = "snooze_retry"

// actionCatalog maps every known action type to its tier. It is the single
// source of truth for "does this action exist and what tier is it."
var actionCatalog = map[string]int{
	ActionTypeQuarantineReplay:         TierAutonomous,
	ActionTypeSnoozeRetry:              TierAutonomous,
	ActionTypeRetryFromFailure:         TierAutonomous,
	ActionTypeRetryCallbacks:           TierAutonomous,
	ActionTypeNotify:                   TierAutonomous,
	ActionTypeEscalate:                 TierAutonomous,
	ActionTypeRerunWithParams:          TierGated,
	ActionTypePauseJob:                 TierGated,
	ActionTypeUnpauseJob:               TierGated,
	ActionTypeClearCacheEntry:          TierGated,
	ActionTypeSuppressDownstreamAlerts: TierGated,
	ActionTypeExtendSLAOnce:            TierGated,
	ActionTypeSkipTask:                 TierApproval,
	ActionTypeOverrideSchemaGate:       TierApproval,
	ActionTypeApplyJobdefPatch:         TierApproval,
}

// ActionTier returns the tier for an action type and whether it is in the
// catalog.
func ActionTier(actionType string) (int, bool) {
	tier, ok := actionCatalog[actionType]
	return tier, ok
}

// ActionParams is the union of typed parameters across the action catalog. Each
// handler reads and validates only the fields it needs.
type ActionParams struct {
	// RunID targets a specific run (retry, snooze, replay, extend_sla). Defaults
	// to the incident's remediation-target run when unset.
	RunID *uuid.UUID `json:"run_id,omitempty"`
	// JobID targets a specific job (pause/unpause, clear_cache, rerun). Defaults
	// to the incident's job when unset.
	JobID *uuid.UUID `json:"job_id,omitempty"`
	// TaskName targets a task (clear_cache_entry). Defaults to the incident task.
	TaskName string `json:"task_name,omitempty"`
	// Channel names a notification channel (notify, escalate).
	Channel string `json:"channel,omitempty"`
	// Message is a notify body.
	Message string `json:"message,omitempty"`
	// Summary is an escalation RCA summary.
	Summary string `json:"summary,omitempty"`
	// Overrides carries whitelisted param overrides (rerun_with_params) or replay
	// --set values (quarantine_replay).
	Overrides map[string]string `json:"overrides,omitempty"`
	// DelaySeconds defers a snooze_retry / bounds a suppress_downstream_alerts
	// window.
	DelaySeconds int64 `json:"delay_seconds,omitempty"`
	// ExtendSeconds extends a per-run SLA once (extend_sla_once).
	ExtendSeconds int64 `json:"extend_seconds,omitempty"`
}

// encode marshals the params for the AgentAction row. It never fails the caller;
// an unmarshalable value yields nil.
func (p ActionParams) encode() datatypes.JSON {
	b, err := json.Marshal(p)
	if err != nil {
		return nil
	}
	return datatypes.JSON(b)
}

func (p ActionParams) delay() time.Duration  { return time.Duration(p.DelaySeconds) * time.Second }
func (p ActionParams) extend() time.Duration { return time.Duration(p.ExtendSeconds) * time.Second }

// snoozePayload is persisted on a snooze_retry durable timer.
type snoozePayload struct {
	RunID uuid.UUID `json:"run_id"`
}

// ActionOps is the server-side operations surface the tier-1/2 catalog dispatches
// onto. It is an interface so the executor is unit-testable with a fake and so
// the incident package does not hard-depend on the run/callback/notification/
// replay/cache subsystems — Stream C wires the concrete adapters. All methods
// map onto machinery that already exists (design action-catalog table).
type ActionOps interface {
	// RetryFromFailure re-runs a failed run through the admit-aware retry entry
	// point (run.Store.RetryFromFailureAdmitted — the B2 safety valves).
	RetryFromFailure(ctx context.Context, runID uuid.UUID) error
	// RetryCallbacks re-runs a run's failed callbacks (Dispatcher.RetryFailed).
	RetryCallbacks(ctx context.Context, runID uuid.UUID) error
	// RerunWithParams starts a new run with whitelisted param overrides, stamped
	// with the incident (new-run semantics: params feed cache identity).
	RerunWithParams(ctx context.Context, jobID uuid.UUID, params map[string]string) (uuid.UUID, error)
	// QuarantineReplay runs a side-effect-free what-if replay with --set params.
	QuarantineReplay(ctx context.Context, runID uuid.UUID, set map[string]string) (json.RawMessage, error)
	// Notify posts a structured update to a notification channel.
	Notify(ctx context.Context, channel, message string) error
	// Escalate pages a channel with an RCA summary.
	Escalate(ctx context.Context, incidentID uuid.UUID, channel, summary string) error
	// SetJobPaused pauses/unpauses a job (Job.Paused).
	SetJobPaused(ctx context.Context, jobID uuid.UUID, paused bool) error
	// ClearCacheEntry deletes a task's cache entry.
	ClearCacheEntry(ctx context.Context, jobID uuid.UUID, taskName string) error
	// SuppressDownstreamAlerts suppresses downstream alerts until a deadline.
	SuppressDownstreamAlerts(ctx context.Context, incidentID uuid.UUID, until time.Time) error
	// ExtendSLAOnce writes a durable per-run SLA override.
	ExtendSLAOnce(ctx context.Context, runID uuid.UUID, extend time.Duration) error
}

// errNoOps signals a dispatching action with no ActionOps configured.
var errNoOps = errors.New("incident: no action ops configured")

// resolveRunID picks the params run id, falling back to the incident's
// remediation target then its run.
func resolveRunID(inc *models.Incident, params ActionParams) (uuid.UUID, error) {
	if params.RunID != nil && *params.RunID != uuid.Nil {
		return *params.RunID, nil
	}
	if inc.RemediationTargetRunID != nil && *inc.RemediationTargetRunID != uuid.Nil {
		return *inc.RemediationTargetRunID, nil
	}
	if inc.RunID != nil && *inc.RunID != uuid.Nil {
		return *inc.RunID, nil
	}
	return uuid.Nil, errors.New("incident: action requires a run id (none in params or incident)")
}

// resolveJobID picks the params job id, falling back to the incident's job.
func resolveJobID(inc *models.Incident, params ActionParams) uuid.UUID {
	if params.JobID != nil && *params.JobID != uuid.Nil {
		return *params.JobID
	}
	return inc.JobID
}

// dispatch executes a permitted tier-1/2 action server-side and returns a JSON
// result to record on the action row. The playbook is consulted only where an
// action needs it (rerun_with_params override whitelist).
func (e *Executor) dispatch(ctx context.Context, actionType string, inc *models.Incident, action *models.AgentAction, params ActionParams, pb Playbook) (map[string]any, error) {
	switch actionType {
	case ActionTypeRetryFromFailure:
		runID, err := resolveRunID(inc, params)
		if err != nil {
			return nil, err
		}
		if e.ops == nil {
			return nil, errNoOps
		}
		if err := e.ops.RetryFromFailure(ctx, runID); err != nil {
			return nil, err
		}
		return map[string]any{"run_id": runID.String()}, nil

	case ActionTypeSnoozeRetry:
		runID, err := resolveRunID(inc, params)
		if err != nil {
			return nil, err
		}
		if params.delay() <= 0 {
			return nil, errors.New("incident: snooze_retry requires a positive delay_seconds")
		}
		fireAt := time.Now().UTC().Add(params.delay())
		timer, err := e.store.ScheduleTimer(ctx, inc.ID, TimerKindSnoozeRetry, fireAt, encodeJSON(snoozePayload{RunID: runID}), &action.ID, inc.Namespace)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"timer_id": timer.ID.String(),
			"run_id":   runID.String(),
			"fire_at":  fireAt.Format(time.RFC3339),
		}, nil

	case ActionTypeRetryCallbacks:
		runID, err := resolveRunID(inc, params)
		if err != nil {
			return nil, err
		}
		if e.ops == nil {
			return nil, errNoOps
		}
		if err := e.ops.RetryCallbacks(ctx, runID); err != nil {
			return nil, err
		}
		return map[string]any{"run_id": runID.String()}, nil

	case ActionTypeNotify:
		if params.Channel == "" {
			return nil, errors.New("incident: notify requires a channel")
		}
		if e.ops == nil {
			return nil, errNoOps
		}
		if err := e.ops.Notify(ctx, params.Channel, params.Message); err != nil {
			return nil, err
		}
		return map[string]any{"channel": params.Channel}, nil

	case ActionTypeEscalate:
		if e.ops == nil {
			return nil, errNoOps
		}
		if err := e.ops.Escalate(ctx, inc.ID, params.Channel, params.Summary); err != nil {
			return nil, err
		}
		return map[string]any{"channel": params.Channel, "escalated": true}, nil

	case ActionTypeRerunWithParams:
		if len(params.Overrides) == 0 {
			return nil, errors.New("incident: rerun_with_params requires overrides")
		}
		if err := validateParamOverrides(params.Overrides, pb.ParamOverrides); err != nil {
			return nil, err
		}
		if e.ops == nil {
			return nil, errNoOps
		}
		jobID := resolveJobID(inc, params)
		newRunID, err := e.ops.RerunWithParams(ctx, jobID, params.Overrides)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"job_id":     jobID.String(),
			"new_run_id": newRunID.String(),
			"overrides":  params.Overrides,
			// New-run semantics: params feed cache identity so the DAG re-keys and
			// recomputes. Disclose the cost on the action record.
			"recompute": true,
		}, nil

	case ActionTypePauseJob:
		if e.ops == nil {
			return nil, errNoOps
		}
		jobID := resolveJobID(inc, params)
		if err := e.ops.SetJobPaused(ctx, jobID, true); err != nil {
			return nil, err
		}
		return map[string]any{"job_id": jobID.String(), "paused": true}, nil

	case ActionTypeUnpauseJob:
		if e.ops == nil {
			return nil, errNoOps
		}
		jobID := resolveJobID(inc, params)
		if err := e.ops.SetJobPaused(ctx, jobID, false); err != nil {
			return nil, err
		}
		return map[string]any{"job_id": jobID.String(), "paused": false}, nil

	case ActionTypeClearCacheEntry:
		if e.ops == nil {
			return nil, errNoOps
		}
		jobID := resolveJobID(inc, params)
		taskName := params.TaskName
		if taskName == "" {
			taskName = inc.TaskName
		}
		if taskName == "" {
			return nil, errors.New("incident: clear_cache_entry requires a task name")
		}
		if err := e.ops.ClearCacheEntry(ctx, jobID, taskName); err != nil {
			return nil, err
		}
		return map[string]any{"job_id": jobID.String(), "task_name": taskName}, nil

	case ActionTypeSuppressDownstreamAlerts:
		if params.delay() <= 0 {
			return nil, errors.New("incident: suppress_downstream_alerts requires a positive delay_seconds")
		}
		if e.ops == nil {
			return nil, errNoOps
		}
		until := time.Now().UTC().Add(params.delay())
		if err := e.ops.SuppressDownstreamAlerts(ctx, inc.ID, until); err != nil {
			return nil, err
		}
		return map[string]any{"until": until.Format(time.RFC3339)}, nil

	case ActionTypeExtendSLAOnce:
		runID, err := resolveRunID(inc, params)
		if err != nil {
			return nil, err
		}
		if params.extend() <= 0 {
			return nil, errors.New("incident: extend_sla_once requires a positive extend_seconds")
		}
		if e.ops == nil {
			return nil, errNoOps
		}
		if err := e.ops.ExtendSLAOnce(ctx, runID, params.extend()); err != nil {
			return nil, err
		}
		return map[string]any{"run_id": runID.String(), "extend_seconds": params.ExtendSeconds}, nil

	case ActionTypeQuarantineReplay:
		runID, err := resolveRunID(inc, params)
		if err != nil {
			return nil, err
		}
		if e.ops == nil {
			return nil, errNoOps
		}
		result, err := e.ops.QuarantineReplay(ctx, runID, params.Overrides)
		if err != nil {
			return nil, err
		}
		out := map[string]any{"run_id": runID.String()}
		if len(result) > 0 {
			out["replay"] = result
		}
		return out, nil

	default:
		// Tier-3 producers reach here only if mis-routed; they must go through the
		// approval gate (decisionApprove), never dispatch. Guard defensively.
		return nil, fmt.Errorf("%w: %q has no autonomous executor (tier-3 or deferred)", ErrUnknownAction, actionType)
	}
}

// validateParamOverrides enforces the rerun_with_params whitelist: every key must
// be whitelisted and its value must be among the allowed values for that key.
func validateParamOverrides(overrides map[string]string, whitelist map[string][]string) error {
	for key, val := range overrides {
		allowed, ok := whitelist[key]
		if !ok {
			return fmt.Errorf("incident: rerun_with_params key %q is not whitelisted", key)
		}
		if len(allowed) == 0 {
			continue
		}
		match := false
		for _, a := range allowed {
			if a == val {
				match = true
				break
			}
		}
		if !match {
			return fmt.Errorf("incident: rerun_with_params value %q for key %q is not whitelisted", val, key)
		}
	}
	return nil
}
