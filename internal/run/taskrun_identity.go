package run

import (
	"errors"
	"fmt"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ErrAmbiguousTaskRun is returned when a (job_run_id, task_id) predicate
// matches more than one TaskRun row. Write paths must then address the
// instance by primary key rather than silently matching the first sibling.
var ErrAmbiguousTaskRun = errors.New("run: multiple task instances match (run, task); TaskRun ID required")

// loadUniqueTaskRun returns the single TaskRun for (runID, taskID).
// Zero rows: gorm.ErrRecordNotFound. Two or more: ErrAmbiguousTaskRun.
// Callers that already hold the TaskRun primary key should query by id.
func loadUniqueTaskRun(db *gorm.DB, runID, taskID uuid.UUID) (*models.TaskRun, error) {
	if db == nil {
		return nil, fmt.Errorf("run: load unique task run: nil db")
	}
	var rows []models.TaskRun
	if err := db.Where("job_run_id = ? AND task_id = ?", runID, taskID).Find(&rows).Error; err != nil {
		return nil, err
	}
	switch len(rows) {
	case 0:
		return nil, gorm.ErrRecordNotFound
	case 1:
		return &rows[0], nil
	default:
		return nil, fmt.Errorf("%w: job_run_id=%s task_id=%s (%d rows)", ErrAmbiguousTaskRun, runID, taskID, len(rows))
	}
}

// loadTaskRunByIDOrUnique resolves a dispatch/complete identity that may be
// either a TaskRun primary key (fan-out instance) or a catalog task ID
// (unfanned unique row). Primary key is tried first.
func loadTaskRunByIDOrUnique(db *gorm.DB, runID, id uuid.UUID) (*models.TaskRun, error) {
	if db == nil {
		return nil, fmt.Errorf("run: load task run: nil db")
	}
	var byPK models.TaskRun
	err := db.Where("id = ? AND job_run_id = ?", id, runID).First(&byPK).Error
	if err == nil {
		return &byPK, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	return loadUniqueTaskRun(db, runID, id)
}

// TaskRunsForTask returns every TaskRun instance of (runID, taskID) ordered by
// partition_index. It is the group-aware read every caller that used to
// arbitrarily `.First()` a sibling must use instead: an unfanned task yields
// exactly one row (identical to the old behavior), a fanned task yields all N so
// the caller can choose the instance it means (e.g. the failed one) or summarize
// the group. Returns an empty slice, never an error, when the task has no rows.
func (s *Store) TaskRunsForTask(runID, taskID uuid.UUID) ([]models.TaskRun, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("run: task runs for task: nil store")
	}
	var rows []models.TaskRun
	if err := s.db.
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		Order("partition_index ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// FailedOrLastTaskRunForTask picks the one instance that best represents a
// (runID, taskID) for failure attribution: the first failed instance in
// partition order, else the first non-successful one, else the first row.
// Incident classification and agent context both need "the instance that
// explains this failure" — reading an arbitrary sibling could classify a group
// from a *succeeded* row.
func FailedOrLastTaskRunForTask(rows []models.TaskRun) *models.TaskRun {
	if len(rows) == 0 {
		return nil
	}
	for i := range rows {
		if TaskStatus(rows[i].Status) == TaskStatusFailed {
			return &rows[i]
		}
	}
	for i := range rows {
		if !IsTerminalSuccess(TaskStatus(rows[i].Status)) {
			return &rows[i]
		}
	}
	return &rows[0]
}

func groupStatusFromInstances(rows []models.TaskRun) TaskStatus {
	if len(rows) == 0 {
		return ""
	}
	allSuccess := true
	anyFailed := false
	allSkipped := true
	allTerminal := true
	for i := range rows {
		st := TaskStatus(rows[i].Status)
		if !IsTerminal(st) {
			allTerminal = false
			allSuccess = false
			allSkipped = false
			continue
		}
		switch st {
		case TaskStatusSucceeded, TaskStatusCached:
			allSkipped = false
		case TaskStatusFailed:
			anyFailed = true
			allSuccess = false
			allSkipped = false
		case TaskStatusSkipped:
			allSuccess = false
		default:
			allSuccess = false
			allSkipped = false
		}
	}
	if !allTerminal {
		return TaskStatusRunning
	}
	if anyFailed {
		return TaskStatusFailed
	}
	if allSkipped {
		return TaskStatusSkipped
	}
	if allSuccess {
		return TaskStatusSucceeded
	}
	return TaskStatusFailed
}

func predecessorGroupSatisfied(rows []models.TaskRun) bool {
	if len(rows) == 0 {
		return false
	}
	for i := range rows {
		if !IsTerminalSuccess(TaskStatus(rows[i].Status)) {
			return false
		}
	}
	return true
}
