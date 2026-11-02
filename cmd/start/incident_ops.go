package start

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	datasetsvc "github.com/caesium-cloud/caesium/api/rest/service/dataset"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/incident"
	internaljobdef "github.com/caesium-cloud/caesium/internal/jobdef"
	jobdiff "github.com/caesium-cloud/caesium/internal/jobdef/diff"
	"github.com/caesium-cloud/caesium/internal/jobdef/docdiff"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/notification"
	"github.com/caesium-cloud/caesium/internal/run"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
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
// only ever run after a human approval, through Executor.ExecuteApproved, and
// release_hold, onto the REST release service. The remaining tier-1/2 methods are
// unreachable until their surface is wired and
// return errIncidentOpNotWired.
type incidentActionOps struct {
	db       *gorm.DB
	runStore *run.Store
	// bus and eventStore are how Escalate DELIVERS. Both may be nil (unit tests,
	// a server started without an event sink); Escalate refuses rather than
	// pretending it escalated when it has no way to reach anyone.
	bus        event.Bus
	eventStore *event.Store
	// holdStore overrides the store a release writes through; nil, as in
	// production, is the default store the REST release uses.
	holdStore *run.Store
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
	return incidentRetryError(err)
}

func incidentRetryError(err error) error {
	// A paused job, an exhausted concurrency slot or a namespace at its run
	// quota is a transient, retryable refusal: surface it as
	// incident.ErrRetryDeferred so a fired snooze_retry timer re-arms instead
	// of dropping the retry, and the action records the deferral.
	if err != nil && (errors.Is(err, run.ErrJobPaused) || errors.Is(err, run.ErrMaxConcurrentRunsReached) || errors.Is(err, run.ErrNamespaceQuota)) {
		return fmt.Errorf("%w: %w", incident.ErrRetryDeferred, err)
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
//
// The returned `routed` closes the remaining half of that gap. Publishing is not
// delivery: the notification subscriber drops an event no NotificationPolicy
// matches, silently. So this reports whether any enabled policy would carry the
// escalation, and dispatch records it on the action row. A deployment with the
// event type unrouted still gets a durable, queryable escalation — it just no
// longer reads as a completed page.
func (o *incidentActionOps) Escalate(ctx context.Context, incidentID uuid.UUID, channel, summary string) (bool, error) {
	if o.bus == nil && o.eventStore == nil {
		return false, fmt.Errorf("%w: no event sink is wired, so an escalation cannot be delivered", errIncidentOpNotWired)
	}

	var inc models.Incident
	if err := o.db.WithContext(ctx).First(&inc, "id = ?", incidentID).Error; err != nil {
		return false, fmt.Errorf("incident: load incident for escalation: %w", err)
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
		return false, fmt.Errorf("incident: encode escalation payload: %w", err)
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
			return false, fmt.Errorf("incident: persist escalation event: %w", err)
		}
	}
	event.PublishAndMarkBusDispatched(ctx, o.bus, o.eventStore, evt)

	// Ask the SAME routing decision the subscriber sends by whether anything
	// will carry this: matching policies in the incident's namespace (or "*")
	// whose channel exists, is enabled and is visible from the policy.
	// A failure to answer is reported as not-routed rather than as an escalation
	// failure: the event is already durable, and overstating delivery is the one
	// outcome to avoid.
	routed := false
	deliveries, err := notification.RouteEvent(ctx, o.db, evt)
	if err != nil {
		log.Warn("incident: could not determine escalation routing",
			"incident_id", incidentID, "error", err)
	} else {
		routed = len(deliveries) > 0
	}
	if !routed {
		log.Warn("incident: escalation raised but no notification policy routes it; nobody was paged",
			"incident_id", incidentID,
			"channel", channel,
			"event_type", string(event.TypeIncidentEscalated),
		)
	}

	log.Warn("incident: escalation raised",
		"incident_id", incidentID,
		"channel", channel,
		"routed", routed,
		"summary", summary,
	)
	return routed, nil
}

func (o *incidentActionOps) SetJobPaused(_ context.Context, _ uuid.UUID, _ bool) error {
	return errIncidentOpNotWired
}

func (o *incidentActionOps) ClearCacheEntry(_ context.Context, _ uuid.UUID, _ string) error {
	return errIncidentOpNotWired
}

// ReleaseHold goes through the service behind POST /v1/datasets/holds/:id/release,
// with that endpoint's preconditions in the same order, so there is one release
// path and one audit trail. The caller is left nil because the action's boundary
// is the incident's, enforced by release.Guard in the release transaction.
func (o *incidentActionOps) ReleaseHold(ctx context.Context, release incident.HoldRelease) error {
	if !datasetsvc.ReleaseAuthenticated() {
		return datasetsvc.ErrHoldReleaseUnauthenticated
	}
	tolerances, err := datasetsvc.ValidateTolerances(release.Tolerate)
	if err != nil {
		return err
	}
	_, err = datasetsvc.NewWithDatabase(ctx, o.db).WithStore(o.holdStore).ReleaseHold(datasetsvc.ReleaseHoldParams{
		HoldID:         release.HoldID,
		Principal:      release.Principal,
		Reason:         release.Reason,
		Tolerances:     tolerances,
		AuditNamespace: release.Namespace,
		Guard:          release.Guard,
	})
	if errors.Is(err, run.ErrDatasetHoldNotActive) {
		return fmt.Errorf("%w: %w", incident.ErrHoldNotActive, err)
	}
	return err
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
// This method only ever sees "render" or "render and apply". A render runs every
// validation the apply does, including the batch validation git sync runs: on the
// Git-PR route a manifest that fails it would stall the source's sync once merged.
//
// namespace is the incident's namespace (identity-and-access A8): the job must
// still live there, the proposed definition must keep it there, and the apply
// is confined to it, so the patch cannot touch a trigger or webhook path that
// also serves another namespace's jobs (B9 trigger tenancy). Both ownership
// checks run before the dry-run return, so the git-synced routes refuse the
// same patches as the direct one.
func (o *incidentActionOps) ApplyJobdefPatch(ctx context.Context, jobID uuid.UUID, namespace string, definition json.RawMessage, dryRun bool) (json.RawMessage, error) {
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
	// The declared-namespace check (B2) runs BEFORE the dry-run return, so the
	// git-synced route refuses an undeclared namespace with the same error as
	// the direct route (whose ValidateBatch runs it again below). Escalating it
	// instead would hand a human a manifest that, once committed, fails git
	// sync's ValidateBatch.
	if err := internaljobdef.ValidateDeclaredNamespaces([]schema.Definition{def}); err != nil {
		return nil, fmt.Errorf("incident: proposed job definition failed server-side validation: %w", err)
	}
	namespace = models.NamespaceOrDefault(namespace)
	if models.NamespaceOrDefault(job.Namespace) != namespace {
		return nil, fmt.Errorf("%w: job %q", incident.ErrNamespaceMoved, job.Alias)
	}
	if models.NamespaceOrDefault(def.Metadata.Namespace) != namespace {
		return nil, fmt.Errorf("%w (incident namespace %q)", incident.ErrPatchMovesNamespace, namespace)
	}

	rendered, err := o.renderJobdefDiff(ctx, &def, job.ID, job.Alias, namespace)
	if err != nil {
		return nil, err
	}
	// The diff was rendered from the job as stored when it was read. A move
	// commits the job's namespace and definition together, so re-reading the
	// namespace after the render refuses a diff that saw the new owner's
	// definition (identity-and-access A8).
	var current models.Job
	if err := o.db.WithContext(ctx).Select("id", "namespace").First(&current, "id = ?", jobID).Error; err != nil {
		return nil, fmt.Errorf("incident: reload job for jobdef patch: %w", err)
	}
	if models.NamespaceOrDefault(current.Namespace) != namespace {
		return nil, fmt.Errorf("%w: job %q", incident.ErrNamespaceMoved, job.Alias)
	}

	importer := internaljobdef.NewImporter(o.db)
	// The error is recorded on the incident's action timeline, which readers
	// confined to the incident's namespace see: another namespace's jobs,
	// datasets and contract findings are redacted from it (A8).
	if err := importer.ValidateBatch(ctx, []schema.Definition{def}); err != nil {
		return nil, patchErrorf(ctx, o.db, namespace, job.Alias, "incident: proposed job definition failed server-side validation", err)
	}
	if dryRun {
		return rendered, nil
	}
	opts := &internaljobdef.ApplyOptions{
		NamespaceAllowlist: []string{namespace},
		ContractAudience:   patchContractAudience(ctx, namespace, job.Alias),
	}
	if _, err := importer.ApplyWithOptions(ctx, &def, opts); err != nil {
		return nil, patchErrorf(ctx, o.db, namespace, job.Alias, "incident: apply proposed job definition", err)
	}
	return rendered, nil
}

// renderJobdefDiff renders the change the proposal makes to the job: a unified
// diff of the job's complete definition against the proposed one, so every field
// a patch writes shows, whichever field that is. The result's `diff` is empty in
// two cases, told apart by `empty`: true when the two definitions are identical,
// false when no diff could be produced, and the caller then shows the proposal.
//
// The current definition is read as a reader confined to the incident's
// namespace (identity-and-access A8): one snapshot, with the configuration of a
// trigger that also serves another namespace's job redacted, because the diff
// lands on the incident's timeline. A job outside that namespace has moved out
// and is refused.
func (o *incidentActionOps) renderJobdefDiff(ctx context.Context, def *schema.Definition, jobID uuid.UUID, alias, namespace string) (json.RawMessage, error) {
	var current *schema.Definition
	scope := internaljobdef.NewTenancyScope([]string{namespace}, nil)
	err := jobdiff.ReadSnapshot(ctx, o.db, func(tx *gorm.DB) error {
		var err error
		current, err = internaljobdef.ExportConfinedTx(tx, jobID.String(), scope, jobdiff.RedactedTriggerConfiguration, func(*models.Job) error {
			return incident.ErrNamespaceMoved
		})
		return err
	})
	if errors.Is(err, incident.ErrNamespaceMoved) {
		return nil, fmt.Errorf("%w: job %q", incident.ErrNamespaceMoved, alias)
	}
	var text string
	if err == nil {
		text, err = definitionDiff(current, def)
	}
	if err != nil {
		log.Warn("incident: could not diff the proposed job definition", "job_alias", alias, "error", err)
	}
	payload := map[string]any{"alias": alias, "empty": err == nil && text == ""}
	if text != "" {
		payload["diff"] = text
	}
	return json.Marshal(payload)
}

func definitionDiff(current, proposed *schema.Definition) (string, error) {
	before, err := decodedDocument(current)
	if err != nil {
		return "", err
	}
	after, err := decodedDocument(proposed)
	if err != nil {
		return "", err
	}
	return docdiff.Documents(before, after)
}

// decodedDocument renders a definition as the manifest decoder reads it. The
// decoder fills step defaults that an exported definition leaves out, and a
// default is not an edit.
func decodedDocument(def *schema.Definition) ([]byte, error) {
	encoded, err := yaml.Marshal(def)
	if err != nil {
		return nil, err
	}
	var decoded schema.Definition
	if err := yaml.Unmarshal(encoded, &decoded); err != nil {
		return nil, err
	}
	return yaml.Marshal(&decoded)
}
