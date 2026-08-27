package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	runsvc "github.com/caesium-cloud/caesium/api/rest/service/run"
	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/atom/docker"
	"github.com/caesium-cloud/caesium/internal/atom/kubernetes"
	"github.com/caesium-cloud/caesium/internal/atom/podman"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

const (
	logHeaderState     = "X-Caesium-Log-State"
	logHeaderSource    = "X-Caesium-Log-Source"
	logHeaderTruncated = "X-Caesium-Log-Truncated"
	// logHeaderTaskRunID names the TaskRun instance whose log is being streamed.
	// It is set on every fan-out response so a client that let the server choose
	// still knows which container it got.
	logHeaderTaskRunID = "X-Caesium-Task-Run-ID"
	// logHeaderPartition carries the streamed instance's partition value.
	logHeaderPartition = "X-Caesium-Partition"
)

// logInstanceLoader reads every TaskRun instance for (run, task). It is a
// package var purely so the handler's fan-out selection can be unit-tested
// without the process-wide default store (which would open a real database).
var logInstanceLoader = func(ctx context.Context, runID, taskID uuid.UUID) ([]*runstorage.TaskRun, error) {
	return runstorage.Default().TaskRunInstances(ctx, runID, taskID)
}

// logSnapshotLoader reads one instance's persisted log snapshot by TaskRun
// primary key. Same test-seam rationale as logInstanceLoader.
var logSnapshotLoader = func(ctx context.Context, runID, taskRunID uuid.UUID) (*runstorage.TaskLogSnapshot, error) {
	return runstorage.Default().TaskLogSnapshotForInstance(ctx, runID, taskRunID)
}

// Logs streams (or replays) one task's container log.
//
//	GET /v1/jobs/:id/runs/:run_id/logs?task_id=<uuid>
//	    [&task_run_id=<uuid>] [&partition=<value>] [&since=<rfc3339>]
//
// # Fan-out contract
//
// A fanned step has N task_runs rows for one (run, task_id) — N containers, N
// logs. `task_id` alone therefore does not name a log stream, and the run-detail
// payload this handler used to match against COLLAPSES the group to one entry
// whose RuntimeID is an arbitrary instance's. Streaming that was silently
// serving the wrong container's output.
//
// So, on a fanned task, the caller must select an instance:
//
//   - `task_run_id=<uuid>` — the TaskRun primary key (from the partitions
//     endpoint / `caesium run partitions --json`). Authoritative.
//   - `partition=<value>` — the partition key, when the value is more convenient
//     than the row id.
//   - neither — 400 with a JSON body listing every instance (partition value,
//     index, status, task_run_id) so the client can retry with one. Failing
//     loudly is deliberate: silently streaming instance 0 is the defect this
//     replaced, and a truncated/wrong log is worse than an error.
//
// An UNFANNED task ignores both selectors and behaves exactly as before.
func Logs(c *echo.Context) error {
	ctx := c.Request().Context()

	jobID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	runID, err := uuid.Parse(c.Param("run_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	taskParam := c.QueryParam("task_id")
	if taskParam == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(fmt.Errorf("task_id is required"))
	}

	taskID, err := uuid.Parse(taskParam)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	var since time.Time
	if sinceParam := c.QueryParam("since"); sinceParam != "" {
		since, err = time.Parse(time.RFC3339Nano, sinceParam)
		if err != nil {
			if parsed, parseErr := time.Parse(time.RFC3339, sinceParam); parseErr == nil {
				since = parsed
			} else {
				return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
			}
		}
	}

	runService := runsvc.New(ctx)

	runEntry, err := runService.Get(runID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	if runEntry.JobID != jobID {
		return echo.ErrNotFound
	}

	var taskEntry *runstorage.TaskRun
	for _, task := range runEntry.Tasks {
		if task != nil && task.ID == taskID {
			taskEntry = task
			break
		}
	}

	if taskEntry == nil {
		return echo.ErrNotFound
	}

	// Fan-out: resolve which instance's log this is. selected is nil for an
	// unfanned task, which keeps the collapsed-entry path below byte-identical.
	selected, problem, err := resolveLogInstance(ctx, runID, taskID, taskEntry,
		strings.TrimSpace(c.QueryParam("task_run_id")), c.QueryParam("partition"))
	if err != nil {
		return err
	}
	if problem != nil {
		// A JSON body (not a bare error string) because the useful part is the
		// instance list the client retries with.
		return c.JSON(problem.Status, problem)
	}

	var snapshot *runstorage.TaskLogSnapshot
	if selected != nil {
		taskEntry = selected
		c.Response().Header().Set(logHeaderTaskRunID, selected.ID.String())
		if selected.PartitionValue != "" {
			c.Response().Header().Set(logHeaderPartition, selected.PartitionValue)
		}
		snapshot, err = logSnapshotLoader(ctx, runID, selected.ID)
	} else {
		snapshot, err = runService.GetTaskLogSnapshot(runID, taskID)
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	if taskEntry.RuntimeID == "" {
		if snapshot != nil {
			return writeLogSnapshot(c, snapshot)
		}
		return writeLogState(c, logStateForTask(taskEntry))
	}

	engine, err := engineFor(ctx, taskEntry.Engine)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	reader, err := engine.Logs(&atom.EngineLogsRequest{ID: taskEntry.RuntimeID, Since: since})
	if err != nil {
		if snapshot != nil {
			return writeLogSnapshot(c, snapshot)
		}
		if taskEntry.CompletedAt != nil || runEntry.CompletedAt != nil {
			return writeLogState(c, "unavailable")
		}
		return echo.NewHTTPError(http.StatusBadGateway, "live log stream unavailable").Wrap(err)
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			log.Error("close log reader", "error", closeErr)
		}
	}()

	res := c.Response()
	res.Header().Set(echo.HeaderContentType, "text/plain; charset=utf-8")
	res.Header().Set(logHeaderSource, "live")
	res.WriteHeader(http.StatusOK)

	flusher, _ := res.(http.Flusher)
	buf := make([]byte, 4096)

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			n, readErr := reader.Read(buf)
			if n > 0 {
				if _, writeErr := res.Write(buf[:n]); writeErr != nil {
					return writeErr
				}
				if flusher != nil {
					flusher.Flush()
				}
			}

			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					return nil
				}

				log.Error(
					"log stream error",
					"job_id", jobID,
					"run_id", runID,
					"task_id", taskID,
					"error", readErr,
				)

				return nil
			}
		}
	}
}

// logInstanceRow is one selectable fan-out instance, echoed in the 400 body when
// a fanned task is requested without a selector.
type logInstanceRow struct {
	TaskRunID string `json:"task_run_id"`
	Partition string `json:"partition"`
	Index     int    `json:"partition_index"`
	Status    string `json:"status"`
}

// resolveLogInstance applies the fan-out selection contract. It returns:
//
//   - (nil, nil) for an unfanned task — the caller keeps its existing behavior;
//   - (instance, nil) when a fanned group's instance was selected;
//   - (nil, *echo.HTTPError) for an unknown selector (404) or an unselected
//     fanned group (400, body listing the instances).
//
// collapsed is the run-detail entry, used to skip the instance query entirely
// for the overwhelmingly common unfanned case: the collapsed payload reports
// PartitionCount == 0 there, so no extra read happens on the hot path.
func resolveLogInstance(
	ctx context.Context,
	runID, taskID uuid.UUID,
	collapsed *runstorage.TaskRun,
	taskRunParam, partitionParam string,
) (*runstorage.TaskRun, *logSelectionProblem, error) {
	if collapsed != nil && collapsed.PartitionCount == 0 && taskRunParam == "" && partitionParam == "" {
		return nil, nil, nil
	}

	instances, err := logInstanceLoader(ctx, runID, taskID)
	if err != nil {
		return nil, nil, echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	if len(instances) == 0 {
		return nil, nil, echo.ErrNotFound
	}

	fanned := len(instances) > 1 || instances[0].PartitionValue != ""

	if taskRunParam != "" {
		taskRunID, parseErr := uuid.Parse(taskRunParam)
		if parseErr != nil {
			return nil, nil, echo.NewHTTPError(http.StatusBadRequest, "task_run_id must be a UUID").Wrap(parseErr)
		}
		for _, inst := range instances {
			if inst.ID == taskRunID {
				return inst, nil, nil
			}
		}
		return nil, &logSelectionProblem{
			Status:    http.StatusNotFound,
			Message:   fmt.Sprintf("task_run_id %s is not an instance of this task in this run", taskRunID),
			Instances: logInstanceRows(instances),
		}, nil
	}

	if partitionParam != "" {
		for _, inst := range instances {
			if inst.PartitionValue == partitionParam {
				return inst, nil, nil
			}
		}
		return nil, &logSelectionProblem{
			Status:    http.StatusNotFound,
			Message:   fmt.Sprintf("task has no partition %q in this run", partitionParam),
			Instances: logInstanceRows(instances),
		}, nil
	}

	if !fanned {
		// A single unfanned row that only reached here because the collapsed
		// entry was unavailable: behave as before.
		return nil, nil, nil
	}

	return nil, &logSelectionProblem{
		Status: http.StatusBadRequest,
		Message: fmt.Sprintf(
			"task is fanned into %d partitions; select one with task_run_id=<uuid> or partition=<value>",
			len(instances)),
		PartitionCount: len(instances),
		Instances:      logInstanceRows(instances),
	}, nil
}

// logSelectionProblem is the JSON body returned when a fan-out log request
// cannot be resolved to one instance. It carries the instance list precisely so
// the client's retry needs no extra round trip.
type logSelectionProblem struct {
	Status         int              `json:"-"`
	Message        string           `json:"message"`
	PartitionCount int              `json:"partition_count,omitempty"`
	Instances      []logInstanceRow `json:"instances"`
}

// logInstanceRows renders the selectable instances, capped so a 10k-partition
// group cannot return a multi-megabyte error body.
func logInstanceRows(instances []*runstorage.TaskRun) []logInstanceRow {
	const maxListed = 200
	rows := make([]logInstanceRow, 0, len(instances))
	for i, inst := range instances {
		if i == maxListed {
			break
		}
		rows = append(rows, logInstanceRow{
			TaskRunID: inst.ID.String(),
			Partition: inst.PartitionValue,
			Index:     inst.PartitionIndex,
			Status:    string(inst.Status),
		})
	}
	return rows
}

func writeLogSnapshot(c *echo.Context, snapshot *runstorage.TaskLogSnapshot) error {
	res := c.Response()
	res.Header().Set(echo.HeaderContentType, "text/plain; charset=utf-8")
	res.Header().Set(logHeaderSource, "persisted")
	if snapshot != nil && snapshot.Truncated {
		res.Header().Set(logHeaderTruncated, "true")
	}
	res.WriteHeader(http.StatusOK)
	if snapshot == nil || snapshot.Text == "" {
		return nil
	}
	_, err := io.Copy(res, strings.NewReader(snapshot.Text))
	return err
}

func writeLogState(c *echo.Context, state string) error {
	c.Response().Header().Set(logHeaderState, state)
	c.Response().WriteHeader(http.StatusNoContent)
	return nil
}

func logStateForTask(task *runstorage.TaskRun) string {
	if task == nil {
		return "unavailable"
	}
	if task.CompletedAt != nil || task.Status == runstorage.TaskStatusSucceeded || task.Status == runstorage.TaskStatusFailed || task.Status == runstorage.TaskStatusSkipped {
		return "empty"
	}
	return "pending"
}

func engineFor(ctx context.Context, engine models.AtomEngine) (atom.Engine, error) {
	switch engine {
	case models.AtomEngineDocker:
		return docker.NewEngine(ctx), nil
	case models.AtomEngineKubernetes:
		return kubernetes.NewEngine(ctx), nil
	case models.AtomEnginePodman:
		return podman.NewEngine(ctx), nil
	default:
		return nil, fmt.Errorf("unsupported engine: %s", engine)
	}
}
