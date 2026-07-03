package incident

import (
	"context"
	"encoding/json"
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
// to close incidents as remediated when a later run succeeds. It never invokes
// an LLM or takes an autonomous action.
type Subscriber struct {
	bus         event.Bus
	db          *gorm.DB
	store       *Store
	classifier  *Classifier
	leaderCheck LeaderCheck
	cooldown    time.Duration
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

// failureContext carries the resolved facts a failure event contributes.
type failureContext struct {
	jobID      uuid.UUID
	taskName   string
	taskRun    *models.TaskRun
	backfillID *uuid.UUID
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
		var tr models.TaskRun
		if err := s.db.WithContext(ctx).
			Where("job_run_id = ? AND task_id = ?", evt.RunID, evt.TaskID).
			First(&tr).Error; err == nil {
			fc.taskRun = &tr
		}
		var task models.Task
		if err := s.db.WithContext(ctx).Select("name").First(&task, "id = ?", evt.TaskID).Error; err == nil {
			fc.taskName = task.Name
		}
	}
	return fc
}

func (s *Subscriber) handleFailure(ctx context.Context, evt event.Event) {
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
		Evidence:               buildEvidence(class, exitCode, sig.Result),
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

// buildEvidence renders a small JSON evidence blob for the incident feed.
func buildEvidence(class FailureClass, exitCode *int, result string) datatypes.JSON {
	m := map[string]any{"class": string(class)}
	if exitCode != nil {
		m["exit_code"] = *exitCode
	}
	if result != "" {
		m["result"] = result
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return datatypes.JSON(b)
}
