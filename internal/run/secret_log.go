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
	updates["log_generation"] = gorm.Expr("CASE WHEN log_scrubbed = ? THEN ? ELSE log_generation END", true, "")
	return updates
}

// SecretLogFence binds incremental log writes to the execution attempt that
// produced them. Claim is nil on the local lane; distributed writes also carry
// the claim generation so a reclaimed worker cannot overwrite its successor.
type SecretLogFence struct {
	Attempt    int
	Claim      *TaskClaim
	Generation string
}

// PrepareSecretTaskLog commits the safe routing bit before a secret-bearing
// runtime starts. Readers must never open its raw runtime log after this write.
func (s *Store) PrepareSecretTaskLog(runID, taskRef uuid.UUID, fence SecretLogFence) error {
	if fence.Generation == "" {
		return ErrTaskClaimMismatch
	}
	return s.writeSecretTaskLog(runID, taskRef, fence, map[string]any{
		"log_scrubbed":   true,
		"log_text":       "",
		"log_truncated":  false,
		"log_generation": fence.Generation,
	}, false)
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
	}, true)
}

func (s *Store) writeSecretTaskLog(runID, taskRef uuid.UUID, fence SecretLogFence, updates map[string]any, requireGeneration bool) error {
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
	if requireGeneration {
		q = q.Where("log_generation = ?", fence.Generation)
	} else {
		// Prepare is idempotent for its own token but cannot replace a producer
		// that already established a different generation under the same
		// attempt/claim identity.
		q = q.Where("(log_generation = ? OR log_generation = ?)", "", fence.Generation)
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

// TaskLogReadState is the cheap cross-node live-log probe. It carries lifecycle
// identity plus bounded snapshot metadata, never log text or resolved values.
type TaskLogReadState struct {
	Status       TaskStatus
	Attempt      int
	ClaimedBy    string
	ClaimAttempt int
	Scrubbed     bool
	Generation   string
	LogBytes     int
	Truncated    bool
}

func (s *Store) TaskLogReadStateForInstance(ctx context.Context, runID, taskRunID uuid.UUID) (*TaskLogReadState, error) {
	var row struct {
		Status        string
		Attempt       int
		ClaimedBy     string
		ClaimAttempt  int
		LogScrubbed   bool
		LogGeneration string
		LogBytes      int
		LogTruncated  bool
	}
	if err := s.db.WithContext(ctx).
		Model(&models.TaskRun{}).
		Select("status", "attempt", "claimed_by", "claim_attempt", "log_scrubbed", "log_generation", taskLogByteLengthExpression(s.db.Dialector.Name())+" AS log_bytes", "log_truncated").
		Where("id = ? AND job_run_id = ?", taskRunID, runID).
		First(&row).Error; err != nil {
		return nil, err
	}
	state := &TaskLogReadState{
		Status: TaskStatus(row.Status), Attempt: row.Attempt,
		ClaimedBy: row.ClaimedBy, ClaimAttempt: row.ClaimAttempt, Scrubbed: row.LogScrubbed,
		Generation: row.LogGeneration, LogBytes: row.LogBytes, Truncated: row.LogTruncated,
	}
	return state, nil
}

func taskLogByteLengthExpression(dialect string) string {
	switch dialect {
	case "dqlite", "sqlite", "sqlite3":
		// SQLite length(TEXT) stops at the first NUL. Casting to BLOB keeps
		// metadata polling sensitive to every byte appended to the snapshot.
		return "length(CAST(log_text AS BLOB))"
	default:
		// PostgreSQL has no BLOB type. OCTET_LENGTH is supported there and is
		// the correct byte-counting operation for multibyte text.
		return "octet_length(log_text)"
	}
}

// SecretTaskLogSnapshotForGeneration loads text only while the producer token
// observed by the metadata probe still owns the row. This closes the gap where
// a replacement can Prepare between the cheap probe and the full-text SELECT.
func (s *Store) SecretTaskLogSnapshotForGeneration(ctx context.Context, runID, taskRunID uuid.UUID, generation string) (*TaskLogSnapshot, error) {
	var row models.TaskRun
	if err := s.db.WithContext(ctx).
		Select("log_text", "log_truncated").
		Where("id = ? AND job_run_id = ? AND log_generation = ? AND log_scrubbed = ?", taskRunID, runID, generation, true).
		First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTaskClaimMismatch
		}
		return nil, err
	}
	if row.LogText == "" && !row.LogTruncated {
		return nil, nil
	}
	return &TaskLogSnapshot{Text: row.LogText, Truncated: row.LogTruncated}, nil
}
