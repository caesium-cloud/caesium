package run

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	runsvc "github.com/caesium-cloud/caesium/api/rest/service/run"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

type partitionRow struct {
	Value       string    `json:"value"`
	Index       int       `json:"index"`
	Status      string    `json:"status"`
	Attempt     int       `json:"attempt"`
	CacheHit    bool      `json:"cache_hit"`
	Duration    string    `json:"duration,omitempty"`
	Error       string    `json:"error,omitempty"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	DependsOn   []string  `json:"depends_on,omitempty"`
	TaskRunID   uuid.UUID `json:"task_run_id"`

	// Absolute RFC3339 timestamps, not only the derived Duration. A skewed
	// group is diagnosed by WHEN its instances ran — a late-dispatched
	// instance and a slow one both show a long wall time, and only the start
	// times tell them apart. Emitted as strings (not *time.Time) so an absent
	// value is omitted outright rather than serialized as a zero timestamp a
	// client renders as 0001-01-01.
	StartedAt   string `json:"started_at,omitempty"`
	CompletedAt string `json:"completed_at,omitempty"`
}

// ListPartitions returns the paginated instance list for a fanned task.
func ListPartitions(c *echo.Context) error {
	ctx := c.Request().Context()
	jobID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}
	runID, err := uuid.Parse(c.Param("run_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}
	taskParam := c.Param("task_id")
	if _, err := jsvc.Service(ctx).Get(jobID); err != nil {
		return echo.ErrNotFound
	}
	runEntry, err := runsvc.New(ctx).Get(runID)
	if err != nil || runEntry.JobID != jobID {
		return echo.ErrNotFound
	}
	taskID, err := resolveTaskRef(jobID, taskParam)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	statusFilter := c.QueryParam("status")
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	offset, _ := strconv.Atoi(c.QueryParam("offset"))
	if offset < 0 {
		offset = 0
	}

	db := runstorage.Default().DB()

	// status_counts is computed over the UNFILTERED group so the UI's status
	// filter can show totals ("3 of 12 failed") while paging a filtered subset.
	var groupRows []models.TaskRun
	if err := db.Model(&models.TaskRun{}).
		Select("status").
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		Find(&groupRows).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	statusCounts := partitionStatusCounts(groupRows)

	q := db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		Order("partition_index ASC")
	if statusFilter != "" {
		q = q.Where("status = ?", statusFilter)
	}
	var rows []models.TaskRun
	if err := q.Offset(offset).Limit(limit).Find(&rows).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	out := projectPartitionRows(rows)
	return c.JSON(http.StatusOK, map[string]any{
		"partitions":    out,
		"total":         len(groupRows),
		"status_counts": statusCounts,
	})
}

// RetryPartition resets a single failed instance of a fanned task.
func RetryPartition(c *echo.Context) error {
	ctx := c.Request().Context()
	jobID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}
	runID, err := uuid.Parse(c.Param("run_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}
	taskParam := c.Param("task_id")
	index, err := strconv.Atoi(c.Param("index"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}
	if _, err := jsvc.Service(ctx).Get(jobID); err != nil {
		return echo.ErrNotFound
	}
	runEntry, err := runsvc.New(ctx).Get(runID)
	if err != nil || runEntry.JobID != jobID {
		return echo.ErrNotFound
	}
	taskID, err := resolveTaskRef(jobID, taskParam)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	db := runstorage.Default().DB()
	var row models.TaskRun
	if err := db.Where("job_run_id = ? AND task_id = ? AND partition_index = ?", runID, taskID, index).
		First(&row).Error; err != nil {
		return echo.ErrNotFound
	}

	// The reset itself is the store's job: it must be transactional, guarded to
	// terminal instances, reset every claim/cache column, re-seed the in-group
	// indegree over non-terminal dependencies, re-open a finished run, and
	// invalidate the owner checkpoints. Doing it here with a bare Updates() did
	// none of that.
	updated, err := runstorage.Default().RetryPartition(ctx, runID, row.ID)
	if err != nil {
		return retryPartitionHTTPError(err)
	}

	return c.JSON(http.StatusOK, map[string]any{
		"retried":     true,
		"index":       index,
		"value":       updated.PartitionValue,
		"task_run_id": updated.ID,
		"status":      string(updated.Status),
	})
}

// partitionStatusCounts is the per-status histogram of a fan-out group, computed
// over the UNFILTERED group so the UI can show "3 of 12 failed" while paging a
// status-filtered subset. A status with no instances is absent rather than zero,
// which keeps the map small for a 10k-instance group.
func partitionStatusCounts(rows []models.TaskRun) map[string]int {
	counts := make(map[string]int, 4)
	for i := range rows {
		counts[rows[i].Status]++
	}
	return counts
}

// projectPartitionRows renders instance rows for the partitions endpoint. Each
// row carries its own TaskRun ID so a client can address exactly one instance
// (retry, logs) rather than the ambiguous catalog task ID.
func projectPartitionRows(rows []models.TaskRun) []partitionRow {
	out := make([]partitionRow, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		pr := partitionRow{
			Value:       r.PartitionValue,
			Index:       r.PartitionIndex,
			Status:      r.Status,
			Attempt:     r.Attempt,
			CacheHit:    r.CacheHit,
			Error:       r.Error,
			Fingerprint: r.PartitionFingerprint,
			TaskRunID:   r.ID,
		}
		if r.StartedAt != nil {
			pr.StartedAt = r.StartedAt.UTC().Format(time.RFC3339)
		}
		if r.CompletedAt != nil {
			pr.CompletedAt = r.CompletedAt.UTC().Format(time.RFC3339)
		}
		if r.StartedAt != nil && r.CompletedAt != nil {
			pr.Duration = r.CompletedAt.Sub(*r.StartedAt).Round(time.Millisecond).String()
		}
		if len(r.PartitionDependsOn) > 0 {
			_ = json.Unmarshal(r.PartitionDependsOn, &pr.DependsOn)
		}
		out = append(out, pr)
	}
	return out
}

// retryPartitionHTTPError maps the store's typed retry failures onto status
// codes. A non-terminal instance is a CONFLICT, not a 500: the caller asked for
// something the current state forbids, and the CLI/UI need to distinguish it
// from a server fault to print a useful message.
func retryPartitionHTTPError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, runstorage.ErrTaskRunNotTerminal):
		return echo.NewHTTPError(http.StatusConflict, "partition is not terminal; only a finished instance can be retried")
	case errors.Is(err, gorm.ErrRecordNotFound):
		return echo.ErrNotFound
	default:
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
}

func resolveTaskRef(jobID uuid.UUID, ref string) (uuid.UUID, error) {
	if id, err := uuid.Parse(ref); err == nil {
		return id, nil
	}
	db := runstorage.Default().DB()
	var task models.Task
	if err := db.Where("job_id = ? AND name = ?", jobID, ref).First(&task).Error; err != nil {
		return uuid.Nil, err
	}
	return task.ID, nil
}
