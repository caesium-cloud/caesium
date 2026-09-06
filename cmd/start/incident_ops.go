package start

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/incident"
	internaljobdef "github.com/caesium-cloud/caesium/internal/jobdef"
	jobdiff "github.com/caesium-cloud/caesium/internal/jobdef/diff"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// errIncidentOpNotWired marks a tier-1/2 action whose concrete server-side
// operation is not yet wired in this phase. The live Phase-0 path only exercises
// RetryFromFailure (the deterministic auto_retry_backoff rule and the
// snooze_retry timer both retry a run); the remaining catalog operations get
// their concrete wiring when Stream C lands the /v1/agent/* endpoint, and no
// surface reaches them until then.
var errIncidentOpNotWired = errors.New("incident: action operation not wired in this phase")

// errSkipTaskNotSkippable is returned when skip_task names a task the run has
// no skippable row for — neither pending (the ordinary skip) nor failed (the
// tier-3 "mark the failed task skipped" case). Returning an error rather than
// silently succeeding is what stops an approved action being recorded
// `executed` when it changed nothing.
var errSkipTaskNotSkippable = errors.New("incident: skip_task found no pending or failed task row for the run")

// errPatchAliasMismatch refuses a jobdef patch whose alias does not match the
// job it targets: the incident boundary applies to the DOCUMENT too, or an
// approved patch for job A could rewrite (or create) job B.
var errPatchAliasMismatch = errors.New("incident: jobdef patch alias does not match the target job")

// incidentActionOps is the server-side implementation of incident.ActionOps.
//
// It backs the durable snooze_retry timer and the deterministic
// auto_retry_backoff rule with the admit-aware retry entry point
// (run.Store.RetryFromFailureAdmitted — the retry safety valves), and the three
// tier-3 operations (skip_task, override_schema_gate, apply_jobdef_patch) that
// only ever run after a human approval, through Executor.ExecuteApproved. The
// remaining tier-1/2 methods are unreachable until their surface is wired and
// return errIncidentOpNotWired.
type incidentActionOps struct {
	db       *gorm.DB
	runStore *run.Store
	// bus and eventStore are how Escalate DELIVERS. Both may be nil (unit tests,
	// a server started without an event sink); Escalate refuses rather than
	// pretending it escalated when it has no way to reach anyone.
	bus        event.Bus
	eventStore *event.Store
}

func newIncidentActionOps(conn *gorm.DB, bus event.Bus, eventStore *event.Store) *incidentActionOps {
	return &incidentActionOps{
		db:         conn,
		runStore:   run.NewStore(conn),
		bus:        bus,
		eventStore: eventStore,
	}
}

func (o *incidentActionOps) RetryFromFailure(_ context.Context, runID uuid.UUID) error {
	_, err := o.runStore.RetryFromFailureAdmitted(runID)
	// A paused job or an exhausted concurrency slot is a transient, retryable
	// refusal: surface it as incident.ErrRetryDeferred so a fired snooze_retry
	// timer re-arms instead of dropping the retry.
	if err != nil && (errors.Is(err, run.ErrJobPaused) || errors.Is(err, run.ErrMaxConcurrentRunsReached)) {
		return fmt.Errorf("%w: %v", incident.ErrRetryDeferred, err)
	}
	return err
}

func (o *incidentActionOps) RetryCallbacks(_ context.Context, _ uuid.UUID) error {
	return errIncidentOpNotWired
}

func (o *incidentActionOps) RerunWithParams(_ context.Context, _ uuid.UUID, _ map[string]string) (uuid.UUID, error) {
	return uuid.Nil, errIncidentOpNotWired
}

func (o *incidentActionOps) QuarantineReplay(_ context.Context, _ uuid.UUID, _ map[string]string) (json.RawMessage, error) {
	return nil, errIncidentOpNotWired
}

func (o *incidentActionOps) Notify(_ context.Context, _, _ string) error {
	return errIncidentOpNotWired
}

// Escalate DELIVERS an escalation by publishing a persisted
// incident_escalated event carrying the incident, the requested channel, and the
// rendered summary (for the apply_jobdef_patch provenance route, the diff).
//
// Publishing rather than calling a sender directly is deliberate: the
// notification subsystem is keyed by channel ID and driven by NotificationPolicy
// rows, and incident_escalated is in notifiableTypes, so an operator routes this
// to Slack/PagerDuty/webhook with the ordinary policy machinery and no new
// plumbing. The event is persisted before it is published, so an escalation
// raised while no subscriber was listening is still queryable from /v1/events.
//
// It returns an error when it cannot publish. That is the point: dispatch
// records the AgentAction `executed` only if this returns nil, so an escalation
// that reached nobody is recorded `failed` instead of claiming a human was
// contacted. Logging and returning success — what this did before — recorded
// `route=escalate` on a page nobody ever received.
func (o *incidentActionOps) Escalate(ctx context.Context, incidentID uuid.UUID, channel, summary string) error {
	if o.bus == nil && o.eventStore == nil {
		return fmt.Errorf("%w: no event sink is wired, so an escalation cannot be delivered", errIncidentOpNotWired)
	}

	var inc models.Incident
	if err := o.db.WithContext(ctx).First(&inc, "id = ?", incidentID).Error; err != nil {
		return fmt.Errorf("incident: load incident for escalation: %w", err)
	}

	payload := map[string]any{
		"incident_id":     inc.ID.String(),
		"job_id":          inc.JobID.String(),
		"incident_class":  inc.Class,
		"incident_status": string(inc.Status),
		"summary":         summary,
	}
	if channel != "" {
		payload["channel"] = channel
	}
	if inc.TaskName != "" {
		payload["task_name"] = inc.TaskName
	}
	// job_alias feeds NotificationPolicy's alias/label filters and the rendered
	// notification body; a lookup failure degrades the payload, never the delivery.
	var job models.Job
	if err := o.db.WithContext(ctx).Select("id", "alias", "labels").First(&job, "id = ?", inc.JobID).Error; err == nil {
		payload["job_alias"] = job.Alias
		if len(job.Labels) > 0 {
			payload["job_labels"] = job.Labels
		}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("incident: encode escalation payload: %w", err)
	}

	evt := event.Event{
		Type:      event.TypeIncidentEscalated,
		JobID:     inc.JobID,
		Timestamp: time.Now().UTC(),
		Payload:   body,
	}
	if inc.RemediationTargetRunID != nil {
		evt.RunID = *inc.RemediationTargetRunID
	} else if inc.RunID != nil {
		evt.RunID = *inc.RunID
	}

	if o.eventStore != nil {
		if err := o.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return o.eventStore.AppendTx(tx, &evt)
		}); err != nil {
			return fmt.Errorf("incident: persist escalation event: %w", err)
		}
	}
	event.PublishAndMarkBusDispatched(ctx, o.bus, o.eventStore, evt)

	log.Warn("incident: escalation raised",
		"incident_id", incidentID,
		"channel", channel,
		"summary", summary,
	)
	return nil
}

func (o *incidentActionOps) SetJobPaused(_ context.Context, _ uuid.UUID, _ bool) error {
	return errIncidentOpNotWired
}

func (o *incidentActionOps) ClearCacheEntry(_ context.Context, _ uuid.UUID, _ string) error {
	return errIncidentOpNotWired
}

func (o *incidentActionOps) SuppressDownstreamAlerts(_ context.Context, _ uuid.UUID, _ time.Time) error {
	return errIncidentOpNotWired
}

func (o *incidentActionOps) ExtendSLAOnce(_ context.Context, _ uuid.UUID, _ time.Duration) error {
	return errIncidentOpNotWired
}

// --- Tier 3 (approval-gated) -------------------------------------------------

// SkipTask marks a task in a run skipped.
//
// Two cases, because the shipped store op covers only one of them:
//
//   - PENDING rows go through run.Store.SkipTask, whose skipTaskAndDescendantsTx
//     honours the successors' trigger rules and emits the task_skipped events —
//     the ordinary "this task is blocking the DAG, move on" case.
//   - A FAILED row is the case the design actually names ("skip_task — mark
//     failed task skipped"), and markTaskSkippedTx is pending-only, so the store
//     call is a no-op for it. The flip is done here, scoped to terminal-failed
//     rows of exactly this (run, task).
//
// The failed→skipped flip deliberately does NOT re-advance successors or emit a
// second task_skipped event: by the time a task is failed, the executor has
// already resolved its descendants, and synthesising a terminal event for a row
// that already emitted task_failed would double-count on every consumer of the
// event stream. What the flip changes is the DAG's own record of the operator's
// decision — which is what the approval was for, and what `caesium why` reads.
func (o *incidentActionOps) SkipTask(ctx context.Context, runID, taskID uuid.UUID, reason string) error {
	if err := o.runStore.SkipTask(runID, taskID, reason); err != nil {
		return err
	}

	var skipped int64
	if err := o.db.WithContext(ctx).
		Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ? AND status = ?", runID, taskID, string(run.TaskStatusSkipped)).
		Count(&skipped).Error; err != nil {
		return err
	}

	res := o.db.WithContext(ctx).
		Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ? AND status = ?", runID, taskID, string(run.TaskStatusFailed)).
		Updates(map[string]any{
			"status":       string(run.TaskStatusSkipped),
			"error":        reason,
			"completed_at": time.Now().UTC(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 && skipped == 0 {
		return fmt.Errorf("%w (run %s, task %s)", errSkipTaskNotSkippable, runID, taskID)
	}
	return nil
}

// OverrideSchemaGateOnce records the one-run output-schema bypass on the run
// row. Both ValidateTaskOutputSchema entry points read it through
// run.SchemaGateOverridden, so the bypass covers fanned and unfanned tasks
// alike. Writing it against a run that no longer exists is an error, not a
// silent success — an approved action that changed nothing must be recorded
// failed.
func (o *incidentActionOps) OverrideSchemaGateOnce(ctx context.Context, runID uuid.UUID) error {
	res := o.db.WithContext(ctx).
		Model(&models.JobRun{}).
		Where("id = ?", runID).
		Updates(map[string]any{
			"schema_gate_override": true,
			"updated_at":           time.Now().UTC(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("incident: override_schema_gate found no run %s", runID)
	}
	return nil
}

// ApplyJobdefPatch renders the proposed definition against the live job and,
// unless dryRun, applies it through the SAME in-process entry point
// POST /v1/jobdefs/apply (and therefore `caesium job apply`) uses —
// internaljobdef.Importer.ApplyWithOptions, preceded by the schema's own
// Validate. Going through the importer rather than hand-writing job rows is what
// keeps an agent-proposed change subject to every apply-time guard: contract
// enforcement, the running-job check, provenance conflict detection.
//
// The provenance route is NOT decided here — Executor.dispatchApplyJobdefPatch
// derives it from the job's git fields and calls this with dryRun accordingly.
// This method only ever sees "render" or "render and apply".
func (o *incidentActionOps) ApplyJobdefPatch(ctx context.Context, jobID uuid.UUID, definition json.RawMessage, dryRun bool) (json.RawMessage, error) {
	var job models.Job
	if err := o.db.WithContext(ctx).First(&job, "id = ?", jobID).Error; err != nil {
		return nil, fmt.Errorf("incident: load job for jobdef patch: %w", err)
	}

	var def schema.Definition
	if err := json.Unmarshal(definition, &def); err != nil {
		return nil, fmt.Errorf("incident: decode proposed job definition: %w", err)
	}
	if err := def.Validate(); err != nil {
		return nil, fmt.Errorf("incident: proposed job definition is invalid: %w", err)
	}
	if def.Metadata.Alias != job.Alias {
		return nil, fmt.Errorf("%w: patch targets %q, job is %q", errPatchAliasMismatch, def.Metadata.Alias, job.Alias)
	}

	rendered, err := o.renderJobdefDiff(ctx, &def, job.Alias)
	if err != nil {
		return nil, err
	}
	if dryRun {
		return rendered, nil
	}

	importer := internaljobdef.NewImporter(o.db)
	if err := importer.ValidateBatch(ctx, []schema.Definition{def}); err != nil {
		return nil, fmt.Errorf("incident: proposed job definition failed server-side validation: %w", err)
	}
	if _, err := importer.ApplyWithOptions(ctx, &def, &internaljobdef.ApplyOptions{}); err != nil {
		return nil, fmt.Errorf("incident: apply proposed job definition: %w", err)
	}
	return rendered, nil
}

// renderJobdefDiff produces the human-readable diff between the proposed
// definition and the job as it stands, SCOPED to the target alias so unrelated
// jobs never appear as deletions (Compare treats every actual spec the desired
// map omits as a delete).
func (o *incidentActionOps) renderJobdefDiff(ctx context.Context, def *schema.Definition, alias string) (json.RawMessage, error) {
	actual, err := jobdiff.LoadDatabaseSpecs(ctx, o.db)
	if err != nil {
		return nil, fmt.Errorf("incident: load current job specs for diff: %w", err)
	}
	scoped := make(map[string]jobdiff.JobSpec, 1)
	if spec, ok := actual[alias]; ok {
		scoped[alias] = spec
	}
	result := jobdiff.Compare(map[string]jobdiff.JobSpec{alias: jobdiff.FromDefinition(def)}, scoped)

	payload := map[string]any{"alias": alias, "empty": result.Empty()}
	if len(result.Updates) > 0 {
		payload["updates"] = result.Updates
	}
	if len(result.Creates) > 0 {
		payload["creates"] = result.Creates
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return out, nil
}
