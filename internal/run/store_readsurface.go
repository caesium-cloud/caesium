package run

import (
	"context"
	"errors"
	"fmt"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// store_readsurface.go holds the read-only queries the *observability* surfaces
// need once a run can carry N TaskRun rows per (job_run_id, task_id).
//
// Every one of these is deliberately instance-addressed. The historic read
// helpers on store.go address task state as
// `WHERE job_run_id = ? AND task_id = ?` + `.First()`, which silently returns an
// arbitrary sibling for a fanned group — the defect class G6 catalogues. These
// helpers either return ALL instances (so the caller can aggregate or choose
// explicitly) or take a TaskRun primary key.
//
// Nothing here writes.

// TaskRunInstances returns every TaskRun row for (runID, taskID) in stable
// partition order (partition_index, then created_at, then id). An unfanned task
// yields exactly one element; a fanned group yields N, including instances that
// were skipped for an in-group dependency failure.
//
// Unlike the collapsed run-detail payload (collapseFanOutGroups), the returned
// rows keep their own TaskRun primary key in ID — the caller can address a
// single instance afterwards.
func (s *Store) TaskRunInstances(ctx context.Context, runID, taskID uuid.UUID) ([]*TaskRun, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("run: task run instances: nil store")
	}

	var rows []models.TaskRun
	if err := s.db.WithContext(ctx).
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		Order("partition_index ASC, created_at ASC, id ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}

	out := make([]*TaskRun, 0, len(rows))
	for i := range rows {
		out = append(out, convertRunTaskModel(&rows[i]))
	}
	return out, nil
}

// TaskLogSnapshotForInstance returns the persisted log snapshot of ONE instance,
// addressed by TaskRun primary key. GetTaskLogSnapshot's (run, task) predicate
// returns an arbitrary sibling's log for a fanned group; this one cannot.
//
// A row with no captured log yields (nil, nil), matching GetTaskLogSnapshot.
func (s *Store) TaskLogSnapshotForInstance(ctx context.Context, runID, taskRunID uuid.UUID) (*TaskLogSnapshot, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("run: task log snapshot: nil store")
	}

	var row models.TaskRun
	if err := s.db.WithContext(ctx).
		Select("log_text", "log_truncated").
		Where("id = ? AND job_run_id = ?", taskRunID, runID).
		First(&row).Error; err != nil {
		return nil, err
	}

	if row.LogText == "" && !row.LogTruncated {
		return nil, nil
	}
	return &TaskLogSnapshot{Text: row.LogText, Truncated: row.LogTruncated}, nil
}

// firstFailedTaskRun returns the lowest-partition-index failed row from an
// already-ordered instance slice, or nil when none failed. It is the shared
// "which instance explains this group's failure" rule.
func firstFailedTaskRun(rows []models.TaskRun) *models.TaskRun {
	for i := range rows {
		if TaskStatus(rows[i].Status) == TaskStatusFailed {
			return &rows[i]
		}
	}
	return nil
}

// loadTaskRunsOrdered reads every (runID, taskID) row in stable partition order
// as raw models, for callers inside package run that need the model columns
// (hash blob, cache origin) rather than the API read model.
func loadTaskRunsOrdered(db *gorm.DB, runID, taskID uuid.UUID) ([]models.TaskRun, error) {
	var rows []models.TaskRun
	if err := db.
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		Order("partition_index ASC, created_at ASC, id ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	return rows, nil
}

// isNotFound is a small readability wrapper used by the read surfaces.
func isNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}
