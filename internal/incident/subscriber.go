package incident

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// LeaderCheck reports whether this node currently hosts the cluster leader. The
// incident subscriber is leader-gated (mirroring the run-queue dequeuer, NOT the
// per-node notification subscriber) so an N-node cluster opens exactly one
// incident per failure. A nil LeaderCheck means "always act" (single-node).
type LeaderCheck func(context.Context) (bool, error)

// classifierFailureTypes are the failure events that can open/append incidents.
var classifierFailureTypes = []event.Type{
	event.TypeTaskFailed,
	event.TypeRunFailed,
	event.TypeRunTimedOut,
	event.TypeSLAMissed,
	event.TypeSchemaViolationRecorded,
}

// successTypes are the events that verify a remediation actually worked: an
// incident may reach `remediated` only when a subsequent run for its job/task
// succeeds.
var successTypes = []event.Type{
	event.TypeTaskSucceeded,
	event.TypeRunCompleted,
}

// Subscriber is the leader-gated incident manager. It consumes failure events,
// classifies each, and opens/correlates an incident; it consumes success events
// to close incidents as remediated when a later run succeeds. When a remediator
// is wired (SetRemediator, behind the master gate) it also runs the Phase-0
// deterministic rules on incident open; it never invokes an LLM.
type Subscriber struct {
	bus         event.Bus
	db          *gorm.DB
	store       *Store
	classifier  *Classifier
	leaderCheck LeaderCheck
	cooldown    time.Duration
	// executor + rules drive the Phase-0 deterministic remediation on incident
	// open. Both are nil unless SetRemediator is called, so default-off
	// deployments and tests take no autonomous action.
	executor *Executor
	rules    *Rules
}

// NewSubscriber constructs an incident subscriber.
func NewSubscriber(bus event.Bus, db *gorm.DB, leaderCheck LeaderCheck, cooldown time.Duration) *Subscriber {
	return &Subscriber{
		bus:         bus,
		db:          db,
		store:       NewStore(db),
		classifier:  NewClassifier(),
		leaderCheck: leaderCheck,
		cooldown:    cooldown,
	}
}

// SetRemediator wires the deterministic-rule executor and rule table so that
// opening an incident whose class maps to a deterministic rule
// (auto_retry_backoff / snooze_until_cron) runs that rule as an actor=policy
// action — the live Phase-0 autonomous path. Only invoked behind the master gate
// (CAESIUM_AGENT_REMEDIATION_ENABLED) from cmd/start. A subscriber without a
// remediator opens and classifies incidents but takes no autonomous action.
func (s *Subscriber) SetRemediator(executor *Executor, rules *Rules) {
	s.executor = executor
	s.rules = rules
}

// Start subscribes to the failure and success event types and processes them
// until ctx is cancelled.
func (s *Subscriber) Start(ctx context.Context) error {
	return s.StartWithReady(ctx, nil)
}

// StartWithReady subscribes and signals readiness once the subscription is live
// (used by tests to avoid a publish race).
func (s *Subscriber) StartWithReady(ctx context.Context, ready chan<- struct{}) error {
	types := append(append([]event.Type{}, classifierFailureTypes...), successTypes...)
	ch, err := s.bus.Subscribe(ctx, event.Filter{Types: types})
	if err != nil {
		return err
	}
	if ready != nil {
		close(ready)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case evt, ok := <-ch:
			if !ok {
				return nil
			}
			s.handle(ctx, evt)
		}
	}
}

// isLeader reports whether this node should act on the event.
func (s *Subscriber) isLeader(ctx context.Context) bool {
	if s.leaderCheck == nil {
		return true
	}
	leader, err := s.leaderCheck(ctx)
	if err != nil {
		log.Warn("incident: leader check failed; skipping event", "error", err)
		return false
	}
	return leader
}

func (s *Subscriber) handle(ctx context.Context, evt event.Event) {
	if evt.Quarantine {
		return
	}
	if !s.isLeader(ctx) {
		return
	}
	switch evt.Type {
	case event.TypeTaskSucceeded, event.TypeRunCompleted:
		s.handleSuccess(ctx, evt)
	default:
		s.handleFailure(ctx, evt)
	}
}

// taskStatusFailed mirrors run.TaskStatusFailed. It is a local constant rather
// than an import so the incident subscriber keeps depending only on models —
// the persisted column value is the contract, and it is covered by the fan-out
// attribution tests.
const taskStatusFailed = "failed"

// failureContext carries the resolved facts a failure event contributes.
type failureContext struct {
	jobID    uuid.UUID
	taskName string
	// taskRun is the instance the incident is classified from. For a fanned
	// step it is the first FAILED instance, never an arbitrary sibling.
	taskRun *models.TaskRun
	// failedPartitions lists every failed instance's partition value when the
	// task is fanned; nil for an unfanned task.
	failedPartitions []string
	backfillID       *uuid.UUID
}

// resolveContext fills in job id, task name, task-run detail, and backfill id
// from the event and the DB.
func (s *Subscriber) resolveContext(ctx context.Context, evt event.Event) failureContext {
	fc := failureContext{jobID: evt.JobID}

	if evt.RunID != uuid.Nil {
		var jr models.JobRun
		if err := s.db.WithContext(ctx).
			Select("id", "job_id", "backfill_id").
			First(&jr, "id = ?", evt.RunID).Error; err == nil {
			if fc.jobID == uuid.Nil {
				fc.jobID = jr.JobID
			}
			fc.backfillID = jr.BackfillID
		}
	}

	if evt.TaskID != uuid.Nil && evt.RunID != uuid.Nil {
		// A fanned step has N task_runs rows for one (run, task). `.First()` on
		// that predicate returns an arbitrary sibling, so an incident could be
		// CLASSIFIED FROM A SUCCEEDED INSTANCE — wrong failure class, wrong
		// error, wrong attempt count, and a remediation dispatched against a row
		// that never failed. Read the whole group in a stable order and attribute
		// to the first FAILED instance; fall back to the lowest-index row only
		// when nothing failed (a run-level event, or a group still settling).
		var rows []models.TaskRun
		if err := s.db.WithContext(ctx).
			Where("job_run_id = ? AND task_id = ?", evt.RunID, evt.TaskID).
			Order("partition_index ASC, created_at ASC, id ASC").
			Find(&rows).Error; err == nil && len(rows) > 0 {
			fc.taskRun = attributionTaskRun(rows)
			fc.failedPartitions = failedPartitionValues(rows)
		}
		var task models.Task
		if err := s.db.WithContext(ctx).Select("name").First(&task, "id = ?", evt.TaskID).Error; err == nil {
			fc.taskName = task.Name
		}
	}
	return fc
}

// attributionTaskRun picks the row an incident is classified from: the first
// failed instance in partition order, else the first row. Returning a pointer
// into rows is safe — rows outlives the call through fc.
func attributionTaskRun(rows []models.TaskRun) *models.TaskRun {
	for i := range rows {
		if rows[i].Status == taskStatusFailed {
			return &rows[i]
		}
	}
	return &rows[0]
}

// failedPartitionValues lists the partition keys of every failed instance, so a
// group failure is reported as "3 of 12 partitions failed" rather than as one
// anonymous task failure. Empty for an unfanned task.
func failedPartitionValues(rows []models.TaskRun) []string {
	if len(rows) == 1 && rows[0].PartitionValue == "" {
		return nil
	}
	out := make([]string, 0, len(rows))
	for i := range rows {
		if rows[i].Status == taskStatusFailed {
			out = append(out, rows[i].PartitionValue)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// runHasTaskAttributedIncident reports whether a task-attributed incident (one
// carrying a resolved TaskID) already exists for the run — i.e. the task_failed
// event already opened the task incident. Suppression keys on this INCIDENT, not
// on the task_runs table, so a task_failed that was dropped (bus buffer
// overflow) or never handled does NOT silently swallow the run failure: with no
// task incident, the run_failed still opens one, preserving remediation dispatch
// and operator visibility. A missed incident is worse than a rare duplicate.
//
// Race-safety: the incident subscriber drains the bus on a single FIFO goroutine
// and task_failed is published before run_failed — the owner finalizes the run
// via store.Complete only AFTER CompleteTaskOwner has committed and published the
// task terminal event (internal/run/owner_manager.go) — so the task-attributed
// incident is already committed and visible by the time run_failed is processed.
//
// On a query error it FAILS OPEN (returns false → the run-level incident opens):
// a rare transient error yielding a rare duplicate is a safer degradation than a
// rare missed incident.
func (s *Subscriber) runHasTaskAttributedIncident(ctx context.Context, runID uuid.UUID) bool {
	var n int64
	if err := s.db.WithContext(ctx).
		Model(&models.Incident{}).
		Where("run_id = ? AND task_id IS NOT NULL", runID).
		Count(&n).Error; err != nil {
		log.Warn("incident: could not check for task-attributed incident; opening run-level incident", "run_id", runID, "error", err)
		return false
	}
	return n > 0
}

func (s *Subscriber) handleFailure(ctx context.Context, evt event.Event) {
	// A failing run publishes BOTH task_failed (carrying the failing TaskID) and
	// run_failed (no TaskID). Both route here, but taskName only resolves for the
	// task-bearing event, so they produce two distinct dedupe keys
	// (job|task|class vs job||class) and open two incidents for one failure —
	// double-counting the failure as two remediation-dispatch candidates and
	// showing a phantom "task unknown" incident. Suppress the redundant run-level
	// twin: when a run_failed carries no task attribution and a task-attributed
	// incident already exists for the run, the task_failed event already opened
	// it. Keying on the incident (not on the failed task_run) means a dropped or
	// unhandled task_failed leaves no incident to suppress against, so the
	// run_failed still opens one — no silently missed incident. Genuinely
	// run-level failures with no task incident (infra/setup errors), and
	// run_timed_out / sla_missed, are preserved and still open a run-level
	// incident.
	if evt.Type == event.TypeRunFailed && evt.TaskID == uuid.Nil && evt.RunID != uuid.Nil {
		if s.runHasTaskAttributedIncident(ctx, evt.RunID) {
			log.Debug("incident: suppressing redundant run-level failure; task-attributed incident exists", "run_id", evt.RunID)
			return
		}
	}

	fc := s.resolveContext(ctx, evt)
	if fc.jobID == uuid.Nil {
		log.Warn("incident: could not resolve job for failure event", "type", evt.Type, "run_id", evt.RunID)
		return
	}

	sig := Signal{EventType: string(evt.Type)}
	var exitCode *int
	if fc.taskRun != nil {
		sig.Result = fc.taskRun.Result
		sig.HasSchemaViolations = len(fc.taskRun.SchemaViolations) > 0
		sig.LogTail = fc.taskRun.LogText
		sig.Error = fc.taskRun.Error
		sig.ExitCode = fc.taskRun.ExitCode
		exitCode = fc.taskRun.ExitCode
	}

	class := s.classifier.Classify(sig)

	var runID *uuid.UUID
	if evt.RunID != uuid.Nil {
		r := evt.RunID
		runID = &r
	}
	var taskID *uuid.UUID
	if evt.TaskID != uuid.Nil {
		t := evt.TaskID
		taskID = &t
	}

	params := OpenParams{
		JobID:                  fc.jobID,
		RunID:                  runID,
		TaskID:                 taskID,
		TaskName:               fc.taskName,
		Class:                  class,
		LastError:              sig.Error,
		Evidence:               buildEvidence(class, exitCode, sig.Result, &fc),
		BackfillID:             fc.backfillID,
		RemediationTargetRunID: runID,
		Cooldown:               s.cooldown,
	}

	inc, outcome, err := s.store.OpenOrAppend(ctx, params)
	if err != nil {
		log.Error("incident: failed to open/append incident", "job_id", fc.jobID, "class", class, "error", err)
		return
	}
	if outcome == OutcomeSuppressed {
		log.Debug("incident: suppressed by cooldown", "job_id", fc.jobID, "class", class)
		return
	}
	metrics.IncidentsTotal.WithLabelValues(string(class), string(inc.Status)).Inc()
	log.Info("incident recorded",
		"incident_id", inc.ID,
		"job_id", fc.jobID,
		"task", fc.taskName,
		"class", class,
		"outcome", outcome,
		"occurrences", inc.OccurrenceCount,
	)

	// Phase-0 deterministic remediation: only on a freshly OPENED incident (not on
	// an appended recurrence, which would double-fire the rule), run the class's
	// deterministic rule as an actor=policy action if one matches and a remediator
	// is wired. Firing only on open keeps the loop bounded: a retried run that
	// fails again appends an occurrence and does not re-fire.
	if outcome == OutcomeOpened && s.executor != nil && s.rules != nil {
		if _, matched, rerr := s.executor.ApplyDeterministicRule(ctx, inc, s.rules); rerr != nil {
			log.Warn("incident: deterministic rule failed", "incident_id", inc.ID, "class", class, "error", rerr)
		} else if matched {
			log.Info("incident: deterministic rule applied", "incident_id", inc.ID, "class", class)
		}
	}
}

// handleSuccess closes open incidents whose job/task later ran green — the
// terminal-verified remediation path.
func (s *Subscriber) handleSuccess(ctx context.Context, evt event.Event) {
	jobID := evt.JobID
	taskName := ""
	if evt.RunID != uuid.Nil {
		var jr models.JobRun
		if err := s.db.WithContext(ctx).Select("id", "job_id").First(&jr, "id = ?", evt.RunID).Error; err == nil && jobID == uuid.Nil {
			jobID = jr.JobID
		}
	}
	if evt.TaskID != uuid.Nil {
		// A success event names the CATALOG task, which for a fanned step covers
		// N instance rows. Under fanOut.failurePolicy: continue the group can be
		// terminal with some partitions failed and some succeeded, so
		// "task_succeeded arrived" is not the same claim as "this task ran
		// green". Remediating on it auto-closed the incident the failed sibling
		// had just opened — a live failure disappearing from every alerting path
		// that watches open incidents, with no human involved.
		//
		// This is belt-and-braces with the store, which no longer emits a
		// group-level task_succeeded for a partially-failed group
		// (internal/run/store.go, groupAllSucceededTx). Both guards are wanted:
		// this one also covers events replayed from history and any future
		// success route.
		if !s.taskRanGreen(ctx, evt) {
			return
		}
		var task models.Task
		if err := s.db.WithContext(ctx).Select("name").First(&task, "id = ?", evt.TaskID).Error; err == nil {
			taskName = task.Name
		}
	}
	if jobID == uuid.Nil {
		return
	}

	incidents, err := s.store.OpenForJobTask(ctx, jobID, taskName)
	if err != nil {
		log.Warn("incident: failed to load open incidents for success", "job_id", jobID, "error", err)
		return
	}
	for i := range incidents {
		inc := &incidents[i]
		remediated, err := s.store.Remediate(ctx, inc.ID, "subsequent run succeeded")
		if err != nil {
			// A concurrent transition or an incident not in a remediable state is
			// non-fatal — just skip it.
			log.Debug("incident: could not remediate on success", "incident_id", inc.ID, "error", err)
			continue
		}
		metrics.IncidentsTotal.WithLabelValues(remediated.Class, string(models.IncidentStatusRemediated)).Inc()
		if !inc.OpenedAt.IsZero() {
			metrics.IncidentResolutionSeconds.WithLabelValues(remediated.Class).Observe(time.Since(inc.OpenedAt).Seconds())
		}
		log.Info("incident remediated", "incident_id", inc.ID, "job_id", jobID, "task", taskName)
	}
}

// evidencePartitionListCap bounds the partition list embedded in an incident's
// evidence blob: a 10k-partition group whose every instance failed must not
// write a 10k-element JSON array into every incident row.
const evidencePartitionListCap = 25

// cappedPartitionList truncates a partition list for embedding, appending a
// "(+N more)" marker so the truncation is visible rather than silent.
func cappedPartitionList(values []string) []string {
	if len(values) <= evidencePartitionListCap {
		return values
	}
	out := make([]string, 0, evidencePartitionListCap+1)
	out = append(out, values[:evidencePartitionListCap]...)
	return append(out, fmt.Sprintf("(+%d more)", len(values)-evidencePartitionListCap))
}

// buildEvidence renders a small JSON evidence blob for the incident feed.
func buildEvidence(class FailureClass, exitCode *int, result string, fc *failureContext) datatypes.JSON {
	m := map[string]any{"class": string(class)}
	if exitCode != nil {
		m["exit_code"] = *exitCode
	}
	if result != "" {
		m["result"] = result
	}
	// Fan-out attribution: name the instance the class was derived from and how
	// many siblings failed, so a group failure is never read as a single
	// anonymous task failure. Both keys are omitted for an unfanned task, so
	// existing evidence blobs are unchanged.
	if fc != nil {
		if fc.taskRun != nil && fc.taskRun.PartitionValue != "" {
			m["partition"] = fc.taskRun.PartitionValue
		}
		if len(fc.failedPartitions) > 0 {
			m["failed_partitions"] = cappedPartitionList(fc.failedPartitions)
			m["failed_partition_count"] = len(fc.failedPartitions)
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return datatypes.JSON(b)
}

// taskRanGreen reports whether a success event may be trusted as "this task ran
// green".
//
// It is a FAN-OUT-ONLY check, deliberately. An unfanned task has exactly one
// TaskRun, the success event and that row are written by the same transaction,
// and the event alone has always been the trigger — so the unfanned path is
// left byte-for-byte as it was, event-trusting, and this returns true without
// consulting the row. Only a fanned step (N rows for one catalog task) can be
// terminal-with-failures while a success event names it, and only there is the
// group's own state the authority. Unreadable or absent rows also return true,
// so a missing row never blocks a remediation that used to happen.
func (s *Subscriber) taskRanGreen(ctx context.Context, evt event.Event) bool {
	if evt.RunID == uuid.Nil {
		return true
	}
	var rows []models.TaskRun
	if err := s.db.WithContext(ctx).
		Select("status", "partition_count").
		Where("job_run_id = ? AND task_id = ?", evt.RunID, evt.TaskID).
		Find(&rows).Error; err != nil || len(rows) == 0 {
		return true
	}
	if len(rows) == 1 && rows[0].PartitionCount == 0 {
		return true
	}
	for i := range rows {
		switch rows[i].Status {
		case "succeeded", "cached":
		default:
			return false
		}
	}
	return true
}
