package incident

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
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

	// Tier 3 — always approval-gated, never auto-executed. Execute() routes them
	// to an ApprovalRequest; ExecuteApproved() is the only path that dispatches
	// them, and only after a human decision (trust-the-substrate C4/C7).
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
	// Reason is the operator-facing justification recorded on a skip_task (it
	// becomes the skipped task row's error text, so the DAG explains itself).
	Reason string `json:"reason,omitempty"`
	// Definition carries the FULL desired job definition for apply_jobdef_patch,
	// in the same schema `caesium job apply` sends (pkg/jobdef.Definition). A
	// whole-document proposal rather than a field patch is deliberate: it is what
	// the shipped diff/apply path consumes, so the human approves exactly the
	// document that will be applied and the rendered diff is the real one.
	Definition json.RawMessage `json:"definition,omitempty"`
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

// snoozePayload is persisted on a snooze_retry durable timer. Rearm counts how
// many times a retryable refusal (e.g. the job was paused when the timer fired)
// re-armed a fresh timer; it is capped so a permanently-paused job cannot loop.
type snoozePayload struct {
	RunID uuid.UUID `json:"run_id"`
	Rearm int       `json:"rearm,omitempty"`
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
	// Escalate pages a channel with an RCA summary, reporting whether the
	// escalation was actually ROUTED to a notification channel. An escalation
	// that reached nobody is still recorded (the event is persisted and
	// queryable), but it must never be reported as delivered — routed=false is
	// how the action row says "raised, but no policy carried it".
	Escalate(ctx context.Context, incidentID uuid.UUID, channel, summary string) (routed bool, err error)
	// SetJobPaused pauses/unpauses a job (Job.Paused).
	SetJobPaused(ctx context.Context, jobID uuid.UUID, paused bool) error
	// ClearCacheEntry deletes a task's cache entry.
	ClearCacheEntry(ctx context.Context, jobID uuid.UUID, taskName string) error
	// SuppressDownstreamAlerts suppresses downstream alerts until a deadline.
	SuppressDownstreamAlerts(ctx context.Context, incidentID uuid.UUID, until time.Time) error
	// ExtendSLAOnce writes a durable per-run SLA override.
	ExtendSLAOnce(ctx context.Context, runID uuid.UUID, extend time.Duration) error

	// --- Tier 3, approval-gated (trust-the-substrate C7) ---------------------
	//
	// These three are reachable ONLY from Executor.ExecuteApproved, i.e. after a
	// human decision recorded on an ApprovalRequest. dispatch() is shared with
	// the autonomous path, but Playbook.decide routes every tier-3 type to
	// decisionApprove, so Execute() can never reach them.

	// SkipTask marks a task in a run skipped. The adapter goes through the
	// shipped run.Store.SkipTask, whose skipTaskAndDescendantsTx honours the
	// successors' trigger rules (design Open Question 3: skip interacts with
	// all_success/all_done and with cache identity — the result payload records
	// that caveat rather than hiding it).
	SkipTask(ctx context.Context, runID, taskID uuid.UUID, reason string) error
	// OverrideSchemaGateOnce records a ONE-RUN output-schema validation bypass on
	// the run row, which both ValidateTaskOutputSchema call sites read.
	OverrideSchemaGateOnce(ctx context.Context, runID uuid.UUID) error
	// ApplyJobdefPatch renders the proposed definition against the live job as a
	// diff and, unless dryRun, applies it through the shipped jobdefs importer
	// (the same in-process entry point POST /v1/jobdefs/apply uses). It returns
	// the rendered diff as JSON so the action row and the escalation carry the
	// exact change a human approved. Provenance routing is NOT its decision —
	// the executor derives the route and calls it with dryRun accordingly.
	ApplyJobdefPatch(ctx context.Context, jobID uuid.UUID, definition json.RawMessage, dryRun bool) (json.RawMessage, error)
}

// errNoOps signals a dispatching action with no ActionOps configured.
var errNoOps = errors.New("incident: no action ops configured")

// ErrCrossBoundaryTarget is returned when an action names a run or job that does
// not belong to the incident's own job. The incident boundary is the security
// boundary: an action allowed for incident X must never reach across it to
// retry, pause, rerun, or clear the cache of an unrelated job/run — even if the
// playbook allows that action type. Recorded as a failed AgentAction.
var ErrCrossBoundaryTarget = errors.New("incident: action target crosses the incident boundary")

// verifyActionBoundary rejects any explicit run/job target that does not belong
// to the incident's job. It runs before every dispatch. Empty targets are
// in-boundary by construction (resolveRunID/resolveJobID fall back to the
// incident's own run/job). Namespace is not yet a column on Job/JobRun (it lands
// with multi-tenancy, design Open Question 4); until then job-id identity is the
// enforced boundary and the incident's namespace travels with its own job.
func (e *Executor) verifyActionBoundary(ctx context.Context, inc *models.Incident, params ActionParams) error {
	if params.JobID != nil && *params.JobID != uuid.Nil && *params.JobID != inc.JobID {
		return fmt.Errorf("%w: job %s is not incident job %s", ErrCrossBoundaryTarget, *params.JobID, inc.JobID)
	}
	if params.RunID != nil && *params.RunID != uuid.Nil {
		var jr models.JobRun
		err := e.store.DB().WithContext(ctx).
			Select("id", "job_id").
			First(&jr, "id = ?", *params.RunID).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("%w: run %s not found", ErrCrossBoundaryTarget, *params.RunID)
			}
			return err
		}
		if jr.JobID != inc.JobID {
			return fmt.Errorf("%w: run %s belongs to job %s, not incident job %s", ErrCrossBoundaryTarget, *params.RunID, jr.JobID, inc.JobID)
		}
	}
	return nil
}

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
	// Enforce the incident boundary before any dispatch: an explicit run/job
	// target must belong to the incident's job.
	if err := e.verifyActionBoundary(ctx, inc, params); err != nil {
		return nil, err
	}
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
		routed, err := e.ops.Escalate(ctx, inc.ID, params.Channel, params.Summary)
		if err != nil {
			return nil, err
		}
		// routed distinguishes "a channel received this" from "the event exists".
		// Recording only escalated:true let an escalation nobody could receive
		// read as a completed page.
		return map[string]any{"channel": params.Channel, "escalated": true, "routed": routed}, nil

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

	case ActionTypeSkipTask:
		runID, err := resolveRunID(inc, params)
		if err != nil {
			return nil, err
		}
		taskID, taskName, err := e.resolveTaskTarget(ctx, inc, params)
		if err != nil {
			return nil, err
		}
		if e.ops == nil {
			return nil, errNoOps
		}
		reason := params.Reason
		if reason == "" {
			reason = fmt.Sprintf("skipped by approved remediation action for incident %s", inc.ID)
		}
		if err := e.ops.SkipTask(ctx, runID, taskID, reason); err != nil {
			return nil, err
		}
		return map[string]any{
			"run_id":    runID.String(),
			"task_id":   taskID.String(),
			"task_name": taskName,
			"reason":    reason,
			// design-agent-in-the-loop.md Open Question 3, recorded on the row
			// rather than left as folklore: a skip changes what downstream sees.
			"caveat": "skipping a task changes downstream trigger-rule evaluation (all_success vs all_done) and the cache identity of consumers that hashed its output",
		}, nil

	case ActionTypeOverrideSchemaGate:
		runID, err := resolveRunID(inc, params)
		if err != nil {
			return nil, err
		}
		if e.ops == nil {
			return nil, errNoOps
		}
		if err := e.ops.OverrideSchemaGateOnce(ctx, runID); err != nil {
			return nil, err
		}
		return map[string]any{
			"run_id": runID.String(),
			"scope":  "one_run",
			"caveat": "output schema violations are not enforced for the remaining tasks of this run; violations are neither recorded nor escalated while the override stands",
		}, nil

	case ActionTypeApplyJobdefPatch:
		return e.dispatchApplyJobdefPatch(ctx, inc, params)

	default:
		// An action type in the catalog with no dispatch arm. Every tier-3 type
		// now has one; this stays as the guard for a future catalog addition that
		// forgets its executor.
		return nil, fmt.Errorf("%w: %q has no autonomous executor", ErrUnknownAction, actionType)
	}
}

// ErrAuthModeNone refuses apply_jobdef_patch under CAESIUM_AUTH_MODE=none (arc
// convention 4). Without an auth mode the approve route is an UNAUTHENTICATED
// POST that the agent container itself — which has network reach to the API —
// could call to approve its own jobdef rewrite. The master gate in
// Environment.Validate only refuses turning remediation ON without an auth mode;
// it does not cover a deployment that enabled remediation and later flipped auth
// off, which is exactly the window this check closes. Recorded as a failed
// AgentAction with this reason: not a panic, not a silent no-op.
var ErrAuthModeNone = errors.New("incident: apply_jobdef_patch refused: an auth mode is required (CAESIUM_AUTH_MODE=none)")

// ErrPatchDefinitionRequired is returned when apply_jobdef_patch carries no
// proposed definition.
var ErrPatchDefinitionRequired = errors.New("incident: apply_jobdef_patch requires a definition")

// ErrPatchAltersRemediation refuses any jobdef patch that would change the
// job's own `metadata.remediation` block.
//
// This is the security boundary that makes a job-level playbook trustworthy at
// all. Since the block is persisted and IS the input to the effective-playbook
// resolver, a patch that edits it is the agent rewriting the policy that governs
// the agent — the design's "the agent may not modify playbooks, profiles" rule,
// one indirection out. The attack is quiet: propose a byte-identical definition
// plus a permissive `metadata.remediation`, and a human approving what looks
// like a no-op hands the agent a wider allowlist for every later proposal.
//
// The refusal is unconditional and covers BOTH provenance routes, so it holds
// whether the patch is applied directly or (for a git-synced job) rendered and
// escalated. A human editing the block through `caesium job apply` is unaffected;
// only the agent's own action surface is refused.
var ErrPatchAltersRemediation = errors.New("incident: apply_jobdef_patch may not change metadata.remediation: an agent may not edit the policy that governs it")

// dispatchApplyJobdefPatch is the provenance router for the tier-3 jobdef patch.
//
// It is enforced SERVER-SIDE and the agent cannot choose the route: for a job
// with authoritative git provenance a direct database apply is refused (the next
// sync cycle would silently revert it and leave Git and the database in
// disagreement), so the approved patch degrades to `escalate` with the rendered
// diff attached. Only a job with no git provenance takes the direct
// diff + apply path.
//
// The Git-PR route (open a PR against the source repo using
// CAESIUM_GIT_WRITE_CREDENTIALS, internal/incident/provenance.go) is
// deliberately NOT here: it belongs to data-circuit-breaker.md Stream F in the
// closed-loop arc. Until it lands, a git-synced job escalates with the diff,
// which is the completed plan's documented no-credentials behaviour.
func (e *Executor) dispatchApplyJobdefPatch(ctx context.Context, inc *models.Incident, params ActionParams) (map[string]any, error) {
	if !authModeActive() {
		return nil, ErrAuthModeNone
	}
	if len(params.Definition) == 0 {
		return nil, ErrPatchDefinitionRequired
	}
	if e.ops == nil {
		return nil, errNoOps
	}

	jobID := resolveJobID(inc, params)
	var job models.Job
	if err := e.store.DB().WithContext(ctx).First(&job, "id = ?", jobID).Error; err != nil {
		return nil, fmt.Errorf("incident: load job for jobdef patch: %w", err)
	}

	// Refuse a self-modifying patch BEFORE the provenance router, so neither the
	// direct route nor the escalate route can carry a policy edit.
	if err := refuseRemediationEdit(job.Remediation, params.Definition); err != nil {
		return nil, err
	}

	if gitSynced(&job) {
		diff, err := e.ops.ApplyJobdefPatch(ctx, jobID, params.Definition, true)
		if err != nil {
			return nil, err
		}
		summary := params.Summary
		if summary == "" {
			summary = fmt.Sprintf("approved jobdef patch for git-synced job %q cannot be applied directly; apply it in the source repository", job.Alias)
		}
		routed, err := e.ops.Escalate(ctx, inc.ID, params.Channel, summary+"\n"+string(diff))
		if err != nil {
			return nil, err
		}
		out := map[string]any{
			"job_id":    jobID.String(),
			"job_alias": job.Alias,
			"route":     routeEscalate,
			"applied":   false,
			"routed":    routed,
			"reason":    "job has authoritative git provenance; a direct apply would be reverted by the next sync",
		}
		if len(diff) > 0 {
			out["diff"] = diff
		}
		return out, nil
	}

	diff, err := e.ops.ApplyJobdefPatch(ctx, jobID, params.Definition, false)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"job_id":    jobID.String(),
		"job_alias": job.Alias,
		"route":     routeDirect,
		"applied":   true,
	}
	if len(diff) > 0 {
		out["diff"] = diff
	}
	return out, nil
}

// refuseRemediationEdit returns ErrPatchAltersRemediation when a proposed
// definition's `metadata.remediation` differs from the one the job currently
// carries. Both sides are canonicalised through the same typed struct, so key
// order and whitespace cannot manufacture a difference.
//
// It deliberately does NOT validate the whole definition: that is the importer's
// job (ApplyJobdefPatch calls Validate before applying), and duplicating it here
// would refuse a policy-identical patch with a schema error from the wrong layer.
// The consequence is that a block differing only in un-normalised whitespace
// reads as a change and is refused — the fail-safe direction, and unreachable in
// practice since the agent proposes the document it was briefed with.
//
// A definition that cannot be parsed at all is refused rather than admitted:
// "unreadable" must not mean "unchanged".
func refuseRemediationEdit(stored datatypes.JSON, definition json.RawMessage) error {
	var def schema.Definition
	if err := json.Unmarshal(definition, &def); err != nil {
		return fmt.Errorf("incident: decode proposed job definition: %w", err)
	}

	current, err := canonicalRemediation(stored)
	if err != nil {
		return fmt.Errorf("incident: decode stored remediation policy: %w", err)
	}
	proposed, err := canonicalRemediation(marshalRemediation(def.Metadata.Remediation))
	if err != nil {
		return fmt.Errorf("incident: encode proposed remediation policy: %w", err)
	}
	if !bytes.Equal(current, proposed) {
		return fmt.Errorf("%w (current %s, proposed %s)",
			ErrPatchAltersRemediation, remediationForMessage(current), remediationForMessage(proposed))
	}
	return nil
}

// canonicalRemediation re-encodes a remediation block through its typed struct so
// two semantically identical documents compare byte-equal. An absent block
// canonicalises to nil, which equals another absent block.
func canonicalRemediation(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var block schema.MetadataRemediation
	if err := json.Unmarshal(raw, &block); err != nil {
		return nil, err
	}
	return json.Marshal(block)
}

func marshalRemediation(block *schema.MetadataRemediation) []byte {
	if block == nil {
		return nil
	}
	encoded, err := json.Marshal(block)
	if err != nil {
		return nil
	}
	return encoded
}

// remediationForMessage renders a canonical block for the refusal message,
// naming the absent case rather than printing empty bytes.
func remediationForMessage(canonical []byte) string {
	if len(canonical) == 0 {
		return "none"
	}
	return string(canonical)
}

// Patch routes recorded on the action result so the timeline says which half of
// the provenance router ran.
const (
	routeDirect   = "direct"
	routeEscalate = "escalate"
)

// authModeActive mirrors the master gate in pkg/env's validate() EXACTLY: an
// auth mode is active when CAESIUM_AUTH_MODE is neither empty nor "none", or an
// SSO provider is configured. Keeping the two conditions identical is what makes
// "the approve route is authenticated" a single rule rather than two that can
// drift; the difference is only WHEN each runs (startup vs. every apply).
func authModeActive() bool {
	vars := env.Variables()
	mode := strings.ToLower(strings.TrimSpace(vars.AuthMode))
	if mode != "" && mode != "none" {
		return true
	}
	return vars.SSOEnabled()
}

// gitSynced reports whether a job's definition is owned by git-sync. Any
// provenance field being set means an external source of truth exists; SourceID
// is the one git-sync always stamps, and the others are checked so a partially
// recorded provenance still routes conservatively (escalate, never apply).
func gitSynced(job *models.Job) bool {
	return strings.TrimSpace(job.ProvenanceSourceID) != "" ||
		strings.TrimSpace(job.ProvenanceRepo) != "" ||
		strings.TrimSpace(job.ProvenanceRef) != "" ||
		strings.TrimSpace(job.ProvenanceCommit) != "" ||
		strings.TrimSpace(job.ProvenancePath) != ""
}

// resolveTaskTarget resolves the skip_task target to a catalog task id + name.
// It prefers the params task name, falling back to the incident's own failing
// task, and refuses a name that does not belong to the incident's job — the
// incident boundary applies to task targets exactly as verifyActionBoundary
// applies it to run/job targets.
func (e *Executor) resolveTaskTarget(ctx context.Context, inc *models.Incident, params ActionParams) (uuid.UUID, string, error) {
	name := strings.TrimSpace(params.TaskName)
	if name == "" {
		name = strings.TrimSpace(inc.TaskName)
	}
	if name == "" {
		if inc.TaskID != nil && *inc.TaskID != uuid.Nil {
			return *inc.TaskID, "", nil
		}
		return uuid.Nil, "", errors.New("incident: skip_task requires a task name (none in params or incident)")
	}
	var task models.Task
	err := e.store.DB().WithContext(ctx).
		Select("id", "name").
		Where("job_id = ? AND name = ?", inc.JobID, name).
		First(&task).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return uuid.Nil, "", fmt.Errorf("%w: task %q is not a task of incident job %s", ErrCrossBoundaryTarget, name, inc.JobID)
		}
		return uuid.Nil, "", err
	}
	return task.ID, task.Name, nil
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
