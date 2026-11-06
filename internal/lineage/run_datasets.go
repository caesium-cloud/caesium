package lineage

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// RunDataset is one persisted lineage dataset identity a run's task runs
// observed. It is the entry point from a run into QueryImpact.
type RunDataset struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Direction is "input" or "output" from the observing step's perspective.
	Direction string `json:"direction"`
	// TaskName is the step that observed the dataset.
	TaskName string `json:"task_name,omitempty"`
	// TaskRunID is the task run the row was persisted for.
	TaskRunID uuid.UUID `json:"task_run_id"`
	// ObservedAt is the created_at of the lineage_datasets row.
	ObservedAt time.Time `json:"observed_at"`
}

// RunDatasets is the response of GET /v1/jobs/:id/runs/:run_id/datasets.
type RunDatasets struct {
	RunID    uuid.UUID    `json:"run_id"`
	JobID    uuid.UUID    `json:"job_id"`
	Datasets []RunDataset `json:"datasets"`
}

// QueryRunDatasets lists the lineage dataset rows persisted for every task run
// of runID. It returns gorm.ErrRecordNotFound when the run does not belong to
// jobID. Datasets are de-duplicated across task-run attempts and partitions on
// (namespace, name, direction, task name), keeping the latest observation, and
// ordered inputs first, then by namespace, name and task.
func QueryRunDatasets(ctx context.Context, db *gorm.DB, jobID, runID uuid.UUID) (*RunDatasets, error) {
	conn := db.WithContext(ctx)

	var count int64
	if err := conn.Table("job_runs").Where("id = ? AND job_id = ?", runID, jobID).Count(&count).Error; err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, gorm.ErrRecordNotFound
	}

	type row struct {
		Namespace string
		Name      string
		Direction string
		TaskName  string
		TaskRunID string
		CreatedAt time.Time
	}
	var rows []row
	err := conn.Table("lineage_datasets ld").
		Select("ld.namespace, ld.name, ld.direction, t.name as task_name, ld.task_run_id, ld.created_at").
		Joins("JOIN task_runs tr ON tr.id = ld.task_run_id").
		Joins("LEFT JOIN tasks t ON t.id = tr.task_id").
		Where("tr.job_run_id = ?", runID).
		Order("ld.created_at DESC, ld.id DESC").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	out := &RunDatasets{RunID: runID, JobID: jobID, Datasets: []RunDataset{}}
	seen := make(map[[4]string]struct{}, len(rows))
	for _, r := range rows {
		key := [4]string{r.Namespace, r.Name, r.Direction, r.TaskName}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		id, _ := uuid.Parse(r.TaskRunID)
		out.Datasets = append(out.Datasets, RunDataset{
			Namespace: r.Namespace, Name: r.Name, Direction: r.Direction,
			TaskName: r.TaskName, TaskRunID: id, ObservedAt: r.CreatedAt,
		})
	}
	slices.SortStableFunc(out.Datasets, func(a, b RunDataset) int {
		if c := strings.Compare(a.Direction, b.Direction); c != 0 {
			return c // "input" sorts before "output"
		}
		if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.TaskName, b.TaskName)
	})
	return out, nil
}
