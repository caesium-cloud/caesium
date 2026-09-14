package run

import (
	"context"
	"errors"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const secretRefPrefix = "secret://"

// TaskSpecHasSecretRefs identifies a task whose frozen execution input can
// resolve secret material. This bit is committed when TaskRun rows are created,
// before a worker can start the task, so the API never races into the raw
// runtime stream while secret resolution is still in progress.
func TaskSpecHasSecretRefs(env map[string]string) bool {
	for _, value := range env {
		if strings.HasPrefix(value, secretRefPrefix) {
			return true
		}
	}
	return false
}

// SaveCapturedTaskLogSnapshot is the final-save seam shared by both executor
// paths. Secret-bearing attempts have already persisted through their fenced
// collector; allowing the generic unfenced save here would let an old attempt
// overwrite a successor after reclaim or retry.
func (s *Store) SaveCapturedTaskLogSnapshot(runID, taskRef uuid.UUID, snapshot *TaskLogSnapshot) error {
	if snapshot == nil {
		return nil
	}
	row, err := loadTaskRunByIDOrUnique(s.db, runID, taskRef)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	// The predicate is part of the write, rather than a prior read, so a worker
	// cannot race PrepareSecretTaskLog and overwrite its sanitized snapshot.
	return s.db.Model(&models.TaskRun{}).
		Where("id = ? AND log_scrubbed = ?", row.ID, false).
		Updates(map[string]any{
			"log_text":      snapshot.Text,
			"log_truncated": snapshot.Truncated,
		}).Error
}

// WithInvalidatedSecretLogSnapshot augments a running-to-pending transition.
// A secret-bearing row must not expose the prior claim's cumulative snapshot
// during the gap before its successor calls PrepareSecretTaskLog. Ordinary log
// snapshots retain their historical behavior.
func WithInvalidatedSecretLogSnapshot(updates map[string]any) map[string]any {
	updates["log_text"] = gorm.Expr("CASE WHEN log_scrubbed = ? THEN ? ELSE log_text END", true, "")
	updates["log_truncated"] = gorm.Expr("CASE WHEN log_scrubbed = ? THEN ? ELSE log_truncated END", true, false)
	return updates
}

// SecretLogFence binds incremental log writes to the execution attempt that
// produced them. Claim is nil on the local lane; distributed writes also carry
// the claim generation so a reclaimed worker cannot overwrite its successor.
type SecretLogFence struct {
	Attempt int
	Claim   *TaskClaim
}

// PrepareSecretTaskLog commits the safe routing bit before a secret-bearing
// runtime starts. Readers must never open its raw runtime log after this write.
func (s *Store) PrepareSecretTaskLog(runID, taskRef uuid.UUID, fence SecretLogFence) error {
	return s.writeSecretTaskLog(runID, taskRef, fence, map[string]any{
		"log_scrubbed":  true,
		"log_text":      "",
		"log_truncated": false,
	})
}

// SaveSecretTaskLogSnapshot replaces the cumulative sanitized snapshot while
// the producing attempt still owns the TaskRun row.
func (s *Store) SaveSecretTaskLogSnapshot(runID, taskRef uuid.UUID, fence SecretLogFence, snapshot *TaskLogSnapshot) error {
	if snapshot == nil {
		return nil
	}
	return s.writeSecretTaskLog(runID, taskRef, fence, map[string]any{
		"log_scrubbed":  true,
		"log_text":      snapshot.Text,
		"log_truncated": snapshot.Truncated,
	})
}

func (s *Store) writeSecretTaskLog(runID, taskRef uuid.UUID, fence SecretLogFence, updates map[string]any) error {
	row, err := loadTaskRunByIDOrUnique(s.db, runID, taskRef)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrTaskClaimMismatch
		}
		return err
	}
	q := s.db.Model(&models.TaskRun{}).
		Where("id = ? AND job_run_id = ? AND attempt = ? AND status IN ?", row.ID, runID, fence.Attempt,
			[]string{string(TaskStatusPending), string(TaskStatusRunning)})
	if fence.Claim != nil {
		q = q.Where("claimed_by = ? AND claim_attempt = ?", fence.Claim.ClaimedBy, fence.Claim.ClaimAttempt)
	}
	res := q.Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrTaskClaimMismatch
	}
	return nil
}

// TaskLogReadState is the cross-node live-log handoff. It carries only the
// bounded sanitized snapshot and lifecycle state, never resolved values.
type TaskLogReadState struct {
	Snapshot     *TaskLogSnapshot
	Status       TaskStatus
	Attempt      int
	ClaimedBy    string
	ClaimAttempt int
	Scrubbed     bool
}

func (s *Store) TaskLogReadStateForInstance(ctx context.Context, runID, taskRunID uuid.UUID) (*TaskLogReadState, error) {
	var row models.TaskRun
	if err := s.db.WithContext(ctx).
		Select("status", "attempt", "claimed_by", "claim_attempt", "log_text", "log_truncated", "log_scrubbed").
		Where("id = ? AND job_run_id = ?", taskRunID, runID).
		First(&row).Error; err != nil {
		return nil, err
	}
	state := &TaskLogReadState{
		Status: TaskStatus(row.Status), Attempt: row.Attempt,
		ClaimedBy: row.ClaimedBy, ClaimAttempt: row.ClaimAttempt, Scrubbed: row.LogScrubbed,
	}
	if row.LogText != "" || row.LogTruncated {
		state.Snapshot = &TaskLogSnapshot{Text: row.LogText, Truncated: row.LogTruncated}
	}
	return state, nil
}
