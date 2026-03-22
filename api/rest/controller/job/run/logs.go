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
)

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

	snapshot, err := runService.GetTaskLogSnapshot(runID, taskID)
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
