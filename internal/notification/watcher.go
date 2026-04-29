package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// slaConfig mirrors pkg/jobdef.SLAConfig for deserialization without
// importing the jobdef package (which would create an import cycle for
// callers that also import models).
type slaConfig struct {
	Duration    time.Duration `json:"duration,omitempty"`
	CompletedBy string        `json:"completedBy,omitempty"`
}

// Watcher periodically scans for timeout and SLA violations,
// publishing the appropriate events on the bus.
type Watcher struct {
	db       *gorm.DB
	bus      event.Bus
	store    *event.Store
	interval time.Duration

	// alertedRuns tracks run-scoped alerts (timeout + duration SLA)
	// to avoid duplicate notifications within a single process lifetime.
	alertedRuns map[uuid.UUID]runAlertState

	// alertedJobs tracks job-scoped alerts (completedBy SLA) keyed
	// by "jobID|YYYY-MM-DD" to dedup per calendar day.
	alertedJobs map[string]struct{}
}

type runAlertState struct {
	timedOut    bool
	slaDuration bool
}

// NewWatcher creates a new timeout/SLA watcher.
func NewWatcher(db *gorm.DB, bus event.Bus, store *event.Store, interval time.Duration) *Watcher {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	return &Watcher{
		db:          db,
		bus:         bus,
		store:       store,
		interval:    interval,
		alertedRuns: make(map[uuid.UUID]runAlertState),
		alertedJobs: make(map[string]struct{}),
	}
}

// Start runs the watcher loop until the context is cancelled.
func (w *Watcher) Start(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			w.scan(ctx)
		}
	}
}

func (w *Watcher) scan(ctx context.Context) {
	now := time.Now().UTC()

	w.scanRunningRuns(ctx, now)
	w.scanCompletedBySLA(ctx, now)
}

// scanRunningRuns checks active runs for timeout and duration-based SLA violations.
func (w *Watcher) scanRunningRuns(ctx context.Context, now time.Time) {
	var runs []models.JobRun
	if err := w.db.WithContext(ctx).
		Preload("Job").
		Where("status = ?", "running").
		Find(&runs).Error; err != nil {
		log.Error("notification watcher: failed to query running runs", "error", err)
		return
	}

	// Clean up alerted state for runs that are no longer running.
	runningIDs := make(map[uuid.UUID]struct{}, len(runs))
	for _, r := range runs {
		runningIDs[r.ID] = struct{}{}
	}
	for id := range w.alertedRuns {
		if _, ok := runningIDs[id]; !ok {
			delete(w.alertedRuns, id)
		}
	}

	for _, r := range runs {
		state := w.alertedRuns[r.ID]

		// Check run timeout.
		if !state.timedOut && r.Job.RunTimeout > 0 {
			deadline := r.StartedAt.Add(r.Job.RunTimeout)
			if now.After(deadline) {
				w.emitTimeoutEvent(ctx, r, now)
				state.timedOut = true
			}
		}

		// Check duration-based SLA.
		sla := parseSLA(r.Job.SLA)
		if !state.slaDuration && sla != nil && sla.Duration > 0 {
			slaDeadline := r.StartedAt.Add(sla.Duration)
			if now.After(slaDeadline) {
				w.emitSLAEvent(ctx, r.JobID, &r, now,
					fmt.Sprintf("SLA duration exceeded: run has been running for %s (limit %s)",
						now.Sub(r.StartedAt).Truncate(time.Second), sla.Duration))
				state.slaDuration = true
			}
		}

		w.alertedRuns[r.ID] = state
	}
}

// scanCompletedBySLA checks all jobs with a completedBy SLA to see if any
// have missed their wall-clock deadline, even if no run was started.
func (w *Watcher) scanCompletedBySLA(ctx context.Context, now time.Time) {
	// Find all non-deleted jobs that have an SLA configured.
	var jobs []models.Job
	if err := w.db.WithContext(ctx).
		Where("sla IS NOT NULL AND sla != 'null' AND sla != '{}'").
		Find(&jobs).Error; err != nil {
		log.Error("notification watcher: failed to query jobs for SLA check", "error", err)
		return
	}

	// First pass: resolve deadlines, collect jobs that need a DB check,
	// and build a single batch query for all of them.
	type candidate struct {
		job         models.Job
		deadline    time.Time
		windowStart time.Time
		alertKey    string
	}
	var candidates []candidate
	var jobIDs []uuid.UUID
	// earliestWindow tracks the oldest window start across all candidates
	// so we can bound the batch query.
	var earliestWindow time.Time

	for _, job := range jobs {
		sla := parseSLA(job.SLA)
		if sla == nil || sla.CompletedBy == "" {
			continue
		}

		deadline, err := resolveCompletedBy(sla.CompletedBy, now)
		if err != nil {
			log.Error("notification watcher: invalid completedBy value",
				"job_alias", job.Alias,
				"completed_by", sla.CompletedBy,
				"error", err,
			)
			continue
		}

		if !now.After(deadline) {
			continue
		}

		alertKey := fmt.Sprintf("%s|%s", job.ID, deadline.Format("2006-01-02"))
		if _, alerted := w.alertedJobs[alertKey]; alerted {
			continue
		}

		windowStart := deadline.Add(-24 * time.Hour)
		if earliestWindow.IsZero() || windowStart.Before(earliestWindow) {
			earliestWindow = windowStart
		}

		candidates = append(candidates, candidate{
			job:         job,
			deadline:    deadline,
			windowStart: windowStart,
			alertKey:    alertKey,
		})
		jobIDs = append(jobIDs, job.ID)
	}

	if len(candidates) == 0 {
		return
	}

	// Single batch query: for each candidate job, find the latest
	// successful run completion time since the earliest window.
	type jobLatest struct {
		JobID       uuid.UUID  `gorm:"column:job_id"`
		LatestCompl time.Time  `gorm:"column:latest_compl"`
	}
	var rows []jobLatest
	if err := w.db.WithContext(ctx).
		Model(&models.JobRun{}).
		Select("job_id, MAX(completed_at) AS latest_compl").
		Where("job_id IN ? AND status = ? AND completed_at >= ?",
			jobIDs, "succeeded", earliestWindow).
		Group("job_id").
		Find(&rows).Error; err != nil {
		log.Error("notification watcher: failed to batch-check completed runs", "error", err)
		return
	}
	latestByJob := make(map[uuid.UUID]time.Time, len(rows))
	for _, row := range rows {
		latestByJob[row.JobID] = row.LatestCompl
	}

	// Second pass: verify each job's latest completion falls within
	// its specific [windowStart, deadline] range.
	for _, c := range candidates {
		if runMetSLA(latestByJob, c.job.ID, c.windowStart, c.deadline) {
			w.alertedJobs[c.alertKey] = struct{}{}
			continue
		}

		w.emitCompletedBySLAEvent(ctx, c.job, c.deadline, now)
		w.alertedJobs[c.alertKey] = struct{}{}
	}

	// Prune old alert keys (keep only today and yesterday).
	today := now.Format("2006-01-02")
	yesterday := now.Add(-24 * time.Hour).Format("2006-01-02")
	for key := range w.alertedJobs {
		parts := strings.SplitN(key, "|", 2)
		if len(parts) == 2 && parts[1] != today && parts[1] != yesterday {
			delete(w.alertedJobs, key)
		}
	}
}

func (w *Watcher) emitTimeoutEvent(ctx context.Context, r models.JobRun, now time.Time) {
	payload := buildRunEventPayload(r, "run timed out after "+r.Job.RunTimeout.String())
	evt := event.Event{
		Type:      event.TypeRunTimedOut,
		JobID:     r.JobID,
		RunID:     r.ID,
		Timestamp: now,
		Payload:   payload,
	}

	w.persistAndPublish(ctx, &evt)
	log.Warn("notification watcher: run timed out",
		"run_id", r.ID,
		"job_alias", r.Job.Alias,
		"timeout", r.Job.RunTimeout,
	)
}

func (w *Watcher) emitSLAEvent(ctx context.Context, jobID uuid.UUID, r *models.JobRun, now time.Time, msg string) {
	var payload json.RawMessage
	if r != nil {
		payload = buildRunEventPayload(*r, msg)
	} else {
		data, _ := json.Marshal(map[string]interface{}{
			"job_id": jobID,
			"error":  msg,
		})
		payload = data
	}

	evt := event.Event{
		Type:      event.TypeSLAMissed,
		JobID:     jobID,
		Timestamp: now,
		Payload:   payload,
	}
	if r != nil {
		evt.RunID = r.ID
	}

	w.persistAndPublish(ctx, &evt)
}

func (w *Watcher) emitCompletedBySLAEvent(ctx context.Context, job models.Job, deadline, now time.Time) {
	msg := fmt.Sprintf("SLA missed: job %q did not complete by %s UTC", job.Alias, deadline.Format("15:04"))
	data, _ := json.Marshal(map[string]interface{}{
		"job_id":     job.ID,
		"job_alias":  job.Alias,
		"job_labels": job.Labels,
		"error":      msg,
		"sla":        map[string]string{"completedBy": deadline.Format("15:04")},
	})

	evt := event.Event{
		Type:      event.TypeSLAMissed,
		JobID:     job.ID,
		Timestamp: now,
		Payload:   data,
	}

	w.persistAndPublish(ctx, &evt)
	log.Warn("notification watcher: completedBy SLA missed",
		"job_alias", job.Alias,
		"deadline", deadline.Format("15:04"),
	)
}

func (w *Watcher) persistAndPublish(ctx context.Context, evt *event.Event) {
	if w.store != nil {
		if err := w.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return w.store.AppendTx(tx, evt)
		}); err != nil {
			log.Error("notification watcher: failed to persist event",
				"event_type", string(evt.Type),
				"error", err,
			)
			return
		}
	}
	w.bus.Publish(*evt)
}

func buildRunEventPayload(r models.JobRun, errorMsg string) json.RawMessage {
	p := map[string]interface{}{
		"id":           r.ID,
		"job_id":       r.JobID,
		"job_alias":    r.Job.Alias,
		"job_labels":   r.Job.Labels,
		"status":       r.Status,
		"started_at":   r.StartedAt,
		"trigger_type": r.TriggerType,
		"error":        errorMsg,
	}
	if r.CompletedAt != nil {
		p["completed_at"] = r.CompletedAt
	}
	if len(r.Params) > 0 {
		var params map[string]string
		if err := json.Unmarshal(r.Params, &params); err == nil && len(params) > 0 {
			p["params"] = params
		}
	}
	data, err := json.Marshal(p)
	if err != nil {
		return nil
	}
	return data
}

// runMetSLA checks whether a job's latest successful run completed within
// its SLA window [windowStart, deadline]. Returns false if no run exists
// for the job or the run completed outside the window.
func runMetSLA(latestByJob map[uuid.UUID]time.Time, jobID uuid.UUID, windowStart, deadline time.Time) bool {
	latest, ok := latestByJob[jobID]
	if !ok {
		return false
	}
	return !latest.Before(windowStart) && !latest.After(deadline)
}

// parseSLA deserializes the job's SLA JSON column.
func parseSLA(raw []byte) *slaConfig {
	if len(raw) == 0 {
		return nil
	}
	var cfg slaConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil
	}
	if cfg.Duration == 0 && cfg.CompletedBy == "" {
		return nil
	}
	return &cfg
}

// resolveCompletedBy parses an "HH:MM" string and returns today's deadline
// in UTC. If the current time is before the deadline, it returns today's
// occurrence; otherwise it returns today's occurrence (already passed).
func resolveCompletedBy(hhmm string, now time.Time) (time.Time, error) {
	parts := strings.SplitN(hhmm, ":", 2)
	if len(parts) != 2 {
		return time.Time{}, fmt.Errorf("expected HH:MM format, got %q", hhmm)
	}
	var hour, min int
	if _, err := fmt.Sscanf(parts[0], "%d", &hour); err != nil || hour < 0 || hour > 23 {
		return time.Time{}, fmt.Errorf("invalid hour in %q", hhmm)
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &min); err != nil || min < 0 || min > 59 {
		return time.Time{}, fmt.Errorf("invalid minute in %q", hhmm)
	}
	y, m, d := now.UTC().Date()
	return time.Date(y, m, d, hour, min, 0, 0, time.UTC), nil
}

// isTimeoutError checks if an error string indicates a run timeout.
func isTimeoutError(errMsg string) bool {
	return strings.HasPrefix(errMsg, "run timed out after ")
}
