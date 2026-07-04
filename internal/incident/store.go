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
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrInvalidTransition is returned when a status transition is not permitted by
// the incident status machine.
var ErrInvalidTransition = errors.New("incident: invalid status transition")

// Store persists incidents. It is the single writer the leader-gated subscriber
// uses to open, correlate, and advance incidents.
type Store struct {
	db *gorm.DB
}

// NewStore returns an incident store over db.
func NewStore(db *gorm.DB) *Store {
	return &Store{db: db}
}

// DB exposes the underlying handle for read APIs built on top of the store.
func (s *Store) DB() *gorm.DB { return s.db }

// OpenParams describes a failure the subscriber wants to record as an incident.
type OpenParams struct {
	Namespace  *string
	JobID      uuid.UUID
	RunID      *uuid.UUID
	TaskID     *uuid.UUID
	TaskName   string
	Class      FailureClass
	LastError  string
	Evidence   datatypes.JSON
	BackfillID *uuid.UUID
	// RemediationTargetRunID is the run whose later success would close this
	// incident as remediated.
	RemediationTargetRunID *uuid.UUID
	// Cooldown suppresses re-opening within this window after the last incident
	// for the same key closed. Zero disables cooldown suppression.
	Cooldown time.Duration
}

// OpenOutcome describes what OpenOrAppend did.
type OpenOutcome string

const (
	// OutcomeOpened: a new incident row was created.
	OutcomeOpened OpenOutcome = "opened"
	// OutcomeAppended: an existing open incident absorbed this as an occurrence.
	OutcomeAppended OpenOutcome = "appended"
	// OutcomeSuppressed: skipped because a same-key incident closed within the
	// cooldown window.
	OutcomeSuppressed OpenOutcome = "suppressed"
)

// OpenOrAppend opens exactly one incident per dedupe key or folds the failure
// into the existing open incident as an occurrence. The open is an ATOMIC
// conditional insert on active_dedupe_key (unique index, ON CONFLICT DO
// NOTHING), so failover races and duplicate per-node sla_missed events cannot
// open twins. A recently-closed same-key incident within Cooldown suppresses a
// fresh open.
func (s *Store) OpenOrAppend(ctx context.Context, p OpenParams) (*models.Incident, OpenOutcome, error) {
	key := DedupeKey(p.JobID, p.TaskName, p.Class)
	now := time.Now().UTC()

	// Cooldown: skip if a same-key incident closed within the window.
	if p.Cooldown > 0 {
		var last models.Incident
		err := s.db.WithContext(ctx).
			Where("dedupe_key = ? AND closed_at IS NOT NULL", key).
			Order("closed_at DESC").
			First(&last).Error
		if err == nil && last.ClosedAt != nil && now.Sub(last.ClosedAt.UTC()) < p.Cooldown {
			return &last, OutcomeSuppressed, nil
		} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, "", err
		}
	}

	// Freeze the agent read-scope allowlist at open. Computed by this
	// leader-gated incident manager (an unscoped server principal) from the
	// lineage-impact graph, EXCLUDING edges derived from the failing run's own
	// outputs. On a fold-in (append) the existing incident keeps its already
	// frozen value; the value computed here is only persisted when this insert
	// wins the atomic conditional create below, so the allowlist is truly frozen
	// at FIRST open. Best-effort: an empty result degrades to the job's own alias.
	allowedJobs := marshalAllowlist(FreezeAllowlist(ctx, s.db, p.JobID, p.RunID))

	activeKey := key
	inc := &models.Incident{
		ID:                     uuid.New(),
		Namespace:              p.Namespace,
		JobID:                  p.JobID,
		RunID:                  p.RunID,
		TaskID:                 p.TaskID,
		TaskName:               p.TaskName,
		Class:                  string(p.Class),
		Status:                 models.IncidentStatusOpen,
		DedupeKey:              key,
		ActiveDedupeKey:        &activeKey,
		OccurrenceCount:        1,
		BackfillID:             p.BackfillID,
		RemediationTargetRunID: p.RemediationTargetRunID,
		AllowedJobs:            allowedJobs,
		LastError:              p.LastError,
		Evidence:               p.Evidence,
		OpenedAt:               now,
		CreatedAt:              now,
		UpdatedAt:              now,
	}

	// Atomic conditional insert with a bounded retry. On conflict with an existing
	// non-terminal incident sharing active_dedupe_key, DoNothing (RowsAffected==0)
	// and fold the failure in as an occurrence. If that twin is transitioned to a
	// terminal state (which nulls active_dedupe_key) between our conflict and the
	// append — a race with the leader's timer supervisor or success path —
	// appendOccurrence finds no active row (ErrRecordNotFound); retry, and the
	// now-free key lets the insert win instead of dropping the failure.
	const maxOpenAttempts = 3
	for attempt := 0; attempt < maxOpenAttempts; attempt++ {
		res := s.db.WithContext(ctx).
			Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "active_dedupe_key"}},
				DoNothing: true,
			}).
			Create(inc)
		if res.Error != nil {
			return nil, "", res.Error
		}
		if res.RowsAffected == 1 {
			return inc, OutcomeOpened, nil
		}

		existing, err := s.appendOccurrence(ctx, key, p, now)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				// The twin closed between the conflict and the append; the dedupe
				// key is free now — retry the insert.
				continue
			}
			return nil, "", err
		}
		return existing, OutcomeAppended, nil
	}
	return nil, "", fmt.Errorf("incident: OpenOrAppend exhausted retries opening incident for dedupe key %q", key)
}

// appendOccurrence increments the occurrence counter on the open incident for
// key and advances its remediation target / last error.
func (s *Store) appendOccurrence(ctx context.Context, key string, p OpenParams, now time.Time) (*models.Incident, error) {
	updates := map[string]any{
		"occurrence_count": gorm.Expr("occurrence_count + 1"),
		"updated_at":       now,
	}
	if p.LastError != "" {
		updates["last_error"] = p.LastError
	}
	if p.RemediationTargetRunID != nil {
		updates["remediation_target_run_id"] = *p.RemediationTargetRunID
	}
	if p.RunID != nil {
		updates["run_id"] = *p.RunID
	}
	if err := s.db.WithContext(ctx).
		Model(&models.Incident{}).
		Where("active_dedupe_key = ?", key).
		Updates(updates).Error; err != nil {
		return nil, err
	}
	var existing models.Incident
	if err := s.db.WithContext(ctx).
		Where("active_dedupe_key = ?", key).
		First(&existing).Error; err != nil {
		return nil, err
	}
	return &existing, nil
}

// Transition advances an incident to a new status, enforcing the status machine.
// On any terminal transition it clears active_dedupe_key (so a future same-key
// failure may open a fresh incident) and stamps closed_at. remediated/escalated
// set closed_at only when they are the final state reached (they still permit a
// later → closed transition, which is a no-op on closed_at).
func (s *Store) Transition(ctx context.Context, id uuid.UUID, to models.IncidentStatus, summary string) (*models.Incident, error) {
	var inc *models.Incident
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var cur models.Incident
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&cur, "id = ?", id).Error; err != nil {
			return err
		}
		if !CanTransition(cur.Status, to) {
			return ErrInvalidTransition
		}
		now := time.Now().UTC()
		updates := map[string]any{
			"status":     to,
			"updated_at": now,
		}
		if summary != "" {
			updates["resolution_summary"] = summary
		}
		// Clearing the active key on any terminal transition frees the dedupe key.
		if to.IsTerminal() {
			updates["active_dedupe_key"] = nil
			if cur.ClosedAt == nil {
				updates["closed_at"] = now
			}
		}
		if err := tx.Model(&models.Incident{}).Where("id = ?", id).Updates(updates).Error; err != nil {
			return err
		}
		// Cancel any pending durable timers owned by this incident on a terminal
		// transition: a stale timer must never fire a retry against a closed
		// incident (A6 invariant).
		if to.IsTerminal() {
			if err := tx.Model(&models.RemediationTimer{}).
				Where("incident_id = ? AND status = ?", id, models.RemediationTimerStatusPending).
				Updates(map[string]any{
					"status":     models.RemediationTimerStatusCancelled,
					"updated_at": now,
				}).Error; err != nil {
				return err
			}
		}
		var reloaded models.Incident
		if err := tx.First(&reloaded, "id = ?", id).Error; err != nil {
			return err
		}
		inc = &reloaded
		return nil
	})
	if err != nil {
		return nil, err
	}
	return inc, nil
}

// Remediate marks an incident remediated then closed in one step. It is the
// terminal-verified success path: the caller invokes it only when a subsequent
// run for the incident's job/task actually succeeded.
func (s *Store) Remediate(ctx context.Context, id uuid.UUID, summary string) (*models.Incident, error) {
	if _, err := s.Transition(ctx, id, models.IncidentStatusRemediated, summary); err != nil {
		return nil, err
	}
	return s.Transition(ctx, id, models.IncidentStatusClosed, summary)
}

// OpenForJobTask returns the open (non-terminal) incidents for a job whose task
// name matches. Used by the terminal-verification success path.
func (s *Store) OpenForJobTask(ctx context.Context, jobID uuid.UUID, taskName string) ([]models.Incident, error) {
	var incidents []models.Incident
	// Match task_name EXACTLY, including the empty string: a run-level success
	// event (run_completed, no TaskID) carries taskName == "" and must remediate
	// only run-level incidents (which store task_name == ""), never every open
	// incident for the job. Per-task success (task_succeeded) carries the task
	// name and remediates that task's incidents. This mirrors the (job, task,
	// class) dedupe correlation key.
	q := s.db.WithContext(ctx).
		Where("job_id = ? AND task_name = ? AND active_dedupe_key IS NOT NULL", jobID, taskName)
	if err := q.Find(&incidents).Error; err != nil {
		return nil, err
	}
	return incidents, nil
}

// Get returns a single incident by id.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (*models.Incident, error) {
	var inc models.Incident
	if err := s.db.WithContext(ctx).First(&inc, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &inc, nil
}

// ScheduleTimer persists a durable timer owned by an incident. The timer
// survives restart/failover so a pending snooze/retry is not lost.
func (s *Store) ScheduleTimer(ctx context.Context, incidentID uuid.UUID, kind string, fireAt time.Time, payload datatypes.JSON, actionID *uuid.UUID, namespace *string) (*models.RemediationTimer, error) {
	now := time.Now().UTC()
	timer := &models.RemediationTimer{
		ID:         uuid.New(),
		Namespace:  namespace,
		IncidentID: incidentID,
		ActionID:   actionID,
		Kind:       kind,
		Payload:    payload,
		Status:     models.RemediationTimerStatusPending,
		FireAt:     fireAt.UTC(),
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.db.WithContext(ctx).Create(timer).Error; err != nil {
		return nil, err
	}
	return timer, nil
}

// CancelTimersForIncident cancels all pending timers for an incident (used on
// human take-over). Terminal transitions already cancel via Transition.
func (s *Store) CancelTimersForIncident(ctx context.Context, incidentID uuid.UUID) (int64, error) {
	res := s.db.WithContext(ctx).
		Model(&models.RemediationTimer{}).
		Where("incident_id = ? AND status = ?", incidentID, models.RemediationTimerStatusPending).
		Updates(map[string]any{
			"status":     models.RemediationTimerStatusCancelled,
			"updated_at": time.Now().UTC(),
		})
	return res.RowsAffected, res.Error
}

// DueTimers returns pending timers whose fire time has passed.
func (s *Store) DueTimers(ctx context.Context, now time.Time, limit int) ([]models.RemediationTimer, error) {
	var timers []models.RemediationTimer
	q := s.db.WithContext(ctx).
		Where("status = ? AND fire_at <= ?", models.RemediationTimerStatusPending, now.UTC()).
		Order("fire_at ASC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Find(&timers).Error; err != nil {
		return nil, err
	}
	return timers, nil
}

// ClaimTimer atomically flips a pending timer to fired, returning true if this
// caller won the claim. The conditional update (status = pending) makes the
// claim safe even if two sweeps race.
func (s *Store) ClaimTimer(ctx context.Context, id uuid.UUID) (bool, error) {
	now := time.Now().UTC()
	res := s.db.WithContext(ctx).
		Model(&models.RemediationTimer{}).
		Where("id = ? AND status = ?", id, models.RemediationTimerStatusPending).
		Updates(map[string]any{
			"status":     models.RemediationTimerStatusFired,
			"fired_at":   now,
			"updated_at": now,
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// AllowedJobsForIncident returns the frozen agent read-scope allowlist for an
// incident (the job aliases the incident manager snapshotted at open). The
// incident's own job alias is guaranteed present when it was resolvable at open.
func (s *Store) AllowedJobsForIncident(ctx context.Context, incidentID uuid.UUID) ([]string, error) {
	var inc models.Incident
	if err := s.db.WithContext(ctx).Select("allowed_jobs").First(&inc, "id = ?", incidentID).Error; err != nil {
		return nil, err
	}
	return unmarshalAllowlist(inc.AllowedJobs), nil
}

// CountActiveAgentSessions returns the number of non-terminal agent sessions
// (pending or running) across all incidents. The leader-gated dispatcher uses
// this to enforce the global concurrent-session cap.
func (s *Store) CountActiveAgentSessions(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.WithContext(ctx).
		Model(&models.AgentSession{}).
		Where("state IN ?", []models.AgentSessionState{
			models.AgentSessionStatePending,
			models.AgentSessionStateRunning,
		}).
		Count(&n).Error
	return n, err
}

// CountActiveAgentSessionsForJob returns the number of non-terminal agent
// sessions bound to incidents of the given job. The dispatcher uses this to
// enforce the per-job concurrent-session cap (default 1).
func (s *Store) CountActiveAgentSessionsForJob(ctx context.Context, jobID uuid.UUID) (int64, error) {
	var n int64
	err := s.db.WithContext(ctx).
		Model(&models.AgentSession{}).
		Joins("JOIN incidents ON incidents.id = agent_sessions.incident_id").
		Where("incidents.job_id = ? AND agent_sessions.state IN ?", jobID, []models.AgentSessionState{
			models.AgentSessionStatePending,
			models.AgentSessionStateRunning,
		}).
		Count(&n).Error
	return n, err
}

// CapExceeded distinguishes which concurrent-session cap a reservation failed.
type CapExceeded int

const (
	// CapNone means the reservation succeeded (no cap exceeded).
	CapNone CapExceeded = iota
	// CapGlobal means the global concurrent-session cap was already reached.
	CapGlobal
	// CapPerJob means the per-job concurrent-session cap was already reached.
	CapPerJob
)

// ReserveAgentSession atomically reserves a slot for a new agent session AT THE
// DATABASE, so the concurrent-session caps hold across processes and nodes — an
// in-process mutex cannot serialize two supervisors on different nodes (or a
// leader-failover split-brain window). It conditionally inserts the pending
// session row in a SINGLE statement, guarded by both the global and per-job
// active-session counts; the count and the insert therefore evaluate atomically
// under SQLite/dqlite's serialized-writer semantics, so two concurrent
// reservations cannot both observe "under cap" and both insert.
//
// The active-session predicate (state IN pending|running) matches
// CountActiveAgentSessions / CountActiveAgentSessionsForJob EXACTLY, so the
// reservation and the counters always agree on what "active" means.
//
// It returns which cap (if any) blocked the reservation. On CapNone the pending
// row identified by session.ID exists and counts as active.
func (s *Store) ReserveAgentSession(ctx context.Context, session *models.AgentSession, jobID uuid.UUID, globalCap, perJobCap int) (CapExceeded, error) {
	pending := string(models.AgentSessionStatePending)
	running := string(models.AgentSessionStateRunning)

	var tokenID any
	if session.TokenID != nil {
		tokenID = *session.TokenID
	}
	var profileID any
	if session.ProfileID != nil {
		profileID = *session.ProfileID
	}
	var namespace any
	if session.Namespace != nil {
		namespace = *session.Namespace
	}

	// Single-statement conditional insert. SQLite/dqlite serializes writers, so
	// the COUNT subqueries and the INSERT are atomic relative to any concurrent
	// reservation on the same database, across processes.
	const q = `
INSERT INTO agent_sessions (id, namespace, incident_id, profile_id, engine, token_id, state, actions_used, tokens_used, created_at, updated_at)
SELECT ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?
WHERE (SELECT COUNT(*) FROM agent_sessions WHERE state IN (?, ?)) < ?
  AND (
    SELECT COUNT(*) FROM agent_sessions ss
    JOIN incidents ii ON ii.id = ss.incident_id
    WHERE ii.job_id = ? AND ss.state IN (?, ?)
  ) < ?`

	res := s.db.WithContext(ctx).Exec(q,
		session.ID, namespace, session.IncidentID, profileID, string(session.Engine), tokenID, string(session.State), session.CreatedAt, session.UpdatedAt,
		pending, running, globalCap,
		jobID, pending, running, perJobCap,
	)
	if res.Error != nil {
		return CapNone, res.Error
	}
	if res.RowsAffected == 1 {
		return CapNone, nil
	}

	// The insert was refused by the guard. Diagnose which cap blocked it so the
	// caller can return the right sentinel. This is post-hoc and purely
	// informational — correctness (no overshoot) already came from the atomic
	// insert above, not from these counts.
	globalActive, err := s.CountActiveAgentSessions(ctx)
	if err != nil {
		return CapGlobal, nil //nolint:nilerr // best-effort diagnosis; reservation already failed
	}
	if globalActive >= int64(globalCap) {
		return CapGlobal, nil
	}
	return CapPerJob, nil
}

// marshalAllowlist encodes a frozen job allowlist as JSON for persistence. An
// empty list is stored as an empty JSON array so the column is unambiguously
// "frozen, and empty" rather than "not yet computed" (NULL).
func marshalAllowlist(jobs []string) datatypes.JSON {
	if jobs == nil {
		jobs = []string{}
	}
	b, err := json.Marshal(jobs)
	if err != nil {
		return nil
	}
	return datatypes.JSON(b)
}

// unmarshalAllowlist decodes a persisted allowlist, tolerating NULL/empty.
func unmarshalAllowlist(raw datatypes.JSON) []string {
	if len(raw) == 0 {
		return nil
	}
	var jobs []string
	if err := json.Unmarshal(raw, &jobs); err != nil {
		return nil
	}
	return jobs
}
