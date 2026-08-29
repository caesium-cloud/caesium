package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	runsvc "github.com/caesium-cloud/caesium/api/rest/service/run"
	internaljob "github.com/caesium-cloud/caesium/internal/job"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// Injectable dependency seams.
//
// The handlers' real dependencies are process-wide singletons resolved from the
// environment (runstorage.Default, jsvc.Service, runsvc.New, env.Variables), so
// a unit test cannot point them at a scratch database and the handler itself
// goes untested — which is exactly how a silently truncating page and a
// 200-that-never-executes both shipped green. These vars keep the HANDLER under
// test; production wiring is unchanged. Mirrors logInstanceLoader in logs.go.
var (
	partitionDB = func() *gorm.DB { return runstorage.Default().DB() }

	partitionJobExists = func(ctx context.Context, jobID uuid.UUID) error {
		_, err := jsvc.Service(ctx).Get(jobID)
		return err
	}

	partitionRunJobID = func(ctx context.Context, runID uuid.UUID) (uuid.UUID, error) {
		entry, err := runsvc.New(ctx).Get(runID)
		if err != nil {
			return uuid.Nil, err
		}
		return entry.JobID, nil
	}

	partitionRetryInstance = func(ctx context.Context, runID, taskRunID uuid.UUID) (*runstorage.TaskRun, int64, error) {
		return runstorage.Default().RetryPartitionVersion(ctx, runID, taskRunID)
	}
	partitionResumeRun = func(j *models.Job, r *models.JobRun, params map[string]string, stateRevision int64) {
		go func() {
			runCtx := runstorage.WithContext(context.Background(), r.ID)
			if err := internaljob.New(j, internaljob.WithTriggerID(nil), internaljob.WithParams(params),
				internaljob.WithExpectedStateRevision(stateRevision)).Run(runCtx); err != nil {
				log.Error("partition retry run failure", "job_id", j.ID, "run_id", r.ID, "error", err)
			}
		}()
	}

	partitionExecutionMode = func() string { return env.Variables().ExecutionMode }
)

const (
	// defaultPartitionPageSize is the page a client gets when it names no limit.
	defaultPartitionPageSize = 100
	// maxPartitionPageSize is the documented ceiling. A larger limit is a client
	// bug worth reporting, not something to silently reduce: a caller that asked
	// for 5000 and got 100 without being told believes it has the whole group.
	maxPartitionPageSize = 1000
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
//
// The envelope carries `total`, `limit`, `offset` and `next_offset` because a
// 10k-partition group does not fit in one response and a client with no
// continuation key cannot tell a complete answer from a truncated one. Those
// four keys are the pagination contract the CLI and UI page on;
// `status_counts` is deliberately NOT paginated (see partitionStatusCounts).
//
// `?partition=<value>` is the keyed read: retrying a partition by the identity
// a human actually knows (`region=eu-west-1`) must not require walking pages
// until the value appears.
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
	if err := partitionJobExists(ctx, jobID); err != nil {
		return echo.ErrNotFound
	}
	runJobID, err := partitionRunJobID(ctx, runID)
	if err != nil || runJobID != jobID {
		return echo.ErrNotFound
	}
	taskID, err := resolveTaskRef(jobID, taskParam)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	statusFilter := c.QueryParam("status")
	partitionFilter := c.QueryParam("partition")

	limit, offset, err := partitionPageBounds(c.QueryParam("limit"), c.QueryParam("offset"))
	if err != nil {
		return err
	}

	db := partitionDB()

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

	filtered := func() *gorm.DB {
		q := db.Model(&models.TaskRun{}).
			Where("job_run_id = ? AND task_id = ?", runID, taskID)
		if statusFilter != "" {
			q = q.Where("status = ?", statusFilter)
		}
		if partitionFilter != "" {
			q = q.Where("partition_value = ?", partitionFilter)
		}
		return q
	}

	// total describes the set the client is PAGING (after status/partition
	// filtering), so total/limit/offset compose into a page count that is
	// actually correct. status_counts above still describes the whole group.
	var total int64
	if err := filtered().Count(&total).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	var rows []models.TaskRun
	if err := filtered().
		Order("partition_index ASC").
		Offset(offset).
		Limit(limit).
		Find(&rows).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	return c.JSON(http.StatusOK, map[string]any{
		"partitions":    projectPartitionRows(rows),
		"total":         int(total),
		"limit":         limit,
		"offset":        offset,
		"next_offset":   nextPartitionOffset(offset, len(rows), int(total)),
		"status_counts": statusCounts,
	})
}

// partitionPageBounds parses and validates the page window. An unparseable or
// out-of-range limit is a 400 rather than a silent fallback: a client that asked
// for 5000 rows and received 100 without being told has an incomplete view it
// believes is complete.
func partitionPageBounds(limitParam, offsetParam string) (limit, offset int, err error) {
	limit = defaultPartitionPageSize
	if raw := strings.TrimSpace(limitParam); raw != "" {
		parsed, convErr := strconv.Atoi(raw)
		if convErr != nil || parsed <= 0 || parsed > maxPartitionPageSize {
			return 0, 0, echo.NewHTTPError(http.StatusBadRequest,
				fmt.Sprintf("limit must be an integer between 1 and %d", maxPartitionPageSize))
		}
		limit = parsed
	}
	if raw := strings.TrimSpace(offsetParam); raw != "" {
		parsed, convErr := strconv.Atoi(raw)
		if convErr != nil || parsed < 0 {
			return 0, 0, echo.NewHTTPError(http.StatusBadRequest, "offset must be a non-negative integer")
		}
		offset = parsed
	}
	return limit, offset, nil
}

// nextPartitionOffset returns the offset a client should request next, or nil
// when this page is the last one. Nil (JSON null) rather than an offset past
// the end so "am I done?" is a null check, not arithmetic the client can get
// wrong.
func nextPartitionOffset(offset, returned, total int) *int {
	next := offset + returned
	if returned == 0 || next >= total {
		return nil
	}
	return &next
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
	if err := partitionJobExists(ctx, jobID); err != nil {
		return echo.ErrNotFound
	}
	runJobID, err := partitionRunJobID(ctx, runID)
	if err != nil || runJobID != jobID {
		return echo.ErrNotFound
	}
	taskID, err := resolveTaskRef(jobID, taskParam)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	// A per-partition retry only means something when SOMETHING will pick the
	// reset row back up. In local execution mode nothing does: the in-process
	// job engine drives its own DAG and exits when the run finishes, and there
	// is no dispatcher poll and no worker claim loop behind it. Resetting an
	// instance there returned 200 and left it PENDING forever — with the run
	// re-opened to `running`, so the run never completed again either. Refuse,
	// and name the path that does work locally.
	if !partitionRetryIsDispatchable() {
		return echo.NewHTTPError(http.StatusConflict,
			"per-partition retry requires distributed execution mode "+
				"(CAESIUM_EXECUTION_MODE=distributed); in local mode nothing dispatches the reset "+
				"instance — retry the run instead")
	}

	db := partitionDB()
	var jobModel models.Job
	if err := db.First(&jobModel, "id = ?", jobID).Error; err != nil {
		return echo.ErrNotFound
	}
	var runModel models.JobRun
	if err := db.First(&runModel, "id = ? AND job_id = ?", runID, jobID).Error; err != nil {
		return echo.ErrNotFound
	}
	params := make(map[string]string)
	if len(runModel.Params) > 0 {
		if err := json.Unmarshal(runModel.Params, &params); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "invalid persisted run parameters").Wrap(err)
		}
	}
	var row models.TaskRun
	if err := db.Where("job_run_id = ? AND task_id = ? AND partition_index = ?", runID, taskID, index).
		First(&row).Error; err != nil {
		return echo.ErrNotFound
	}

	// The reset itself is the store's job: it must be transactional, guarded to
	// FAILED instances, reset every claim/cache/output column, re-seed the
	// in-group indegree over non-terminal dependencies, re-open a finished run,
	// and invalidate the owner checkpoints. Doing it here with a bare Updates()
	// did none of that.
	updated, stateRevision, err := partitionRetryInstance(ctx, runID, row.ID)
	if err != nil {
		return retryPartitionHTTPError(err)
	}
	partitionResumeRun(&jobModel, &runModel, params, stateRevision)

	return c.JSON(http.StatusOK, map[string]any{
		"retried":     true,
		"index":       index,
		"value":       updated.PartitionValue,
		"task_run_id": updated.ID,
		"status":      string(updated.Status),
	})
}

// partitionRetryIsDispatchable reports whether this server has a lane that will
// actually dispatch a reset instance. Mirrors the replay service's
// isDistributedExecutionMode: the same "nothing polls in local mode" constraint,
// resolved from the same env var.
func partitionRetryIsDispatchable() bool {
	return strings.EqualFold(strings.TrimSpace(partitionExecutionMode()), "distributed")
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
// codes. A non-retryable instance is a CONFLICT, not a 500: the caller asked for
// something the current state forbids, and the CLI/UI need to distinguish it
// from a server fault to print a useful message.
func retryPartitionHTTPError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, runstorage.ErrTaskRunNotTerminal):
		return echo.NewHTTPError(http.StatusConflict, "partition is not terminal; only a failed instance can be retried")
	case errors.Is(err, runstorage.ErrPartitionNotRetryable):
		return echo.NewHTTPError(http.StatusConflict,
			"only a failed partition can be retried; a succeeded or cached instance would discard a result "+
				"downstream steps already consumed, and a skipped or cancelled one was resolved deliberately "+
				"(retry the run to re-run skipped work)")
	case errors.Is(err, runstorage.ErrRunNotTerminal):
		return echo.NewHTTPError(http.StatusConflict, "partition retry requires a failed or succeeded run")
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
	var task models.Task
	if err := partitionDB().Where("job_id = ? AND name = ?", jobID, ref).First(&task).Error; err != nil {
		return uuid.Nil, err
	}
	return task.ID, nil
}
