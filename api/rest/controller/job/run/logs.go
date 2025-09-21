package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/atom/docker"
	"github.com/caesium-cloud/caesium/internal/atom/kubernetes"
	"github.com/caesium-cloud/caesium/internal/atom/podman"
	"github.com/caesium-cloud/caesium/internal/models"
	runstore "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

func Logs(c echo.Context) error {
	ctx := c.Request().Context()

	jobID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.ErrBadRequest.SetInternal(err)
	}

	runID, err := uuid.Parse(c.Param("run_id"))
	if err != nil {
		return echo.ErrBadRequest.SetInternal(err)
	}

	taskParam := c.QueryParam("task_id")
	if taskParam == "" {
		return echo.ErrBadRequest.SetInternal(fmt.Errorf("task_id is required"))
	}

	taskID, err := uuid.Parse(taskParam)
	if err != nil {
		return echo.ErrBadRequest.SetInternal(err)
	}

	runEntry, ok := runstore.Default().Get(runID)
	if !ok || runEntry.JobID != jobID {
		return echo.ErrNotFound
	}

	var taskEntry *runstore.Task
	for _, task := range runEntry.Tasks {
		if task != nil && task.ID == taskID {
			taskEntry = task
			break
		}
	}

	if taskEntry == nil {
		return echo.ErrNotFound
	}

	if taskEntry.RuntimeID == "" {
		return echo.NewHTTPError(http.StatusConflict, "task has not started")
	}

	engine, err := engineFor(ctx, taskEntry.Engine)
	if err != nil {
		return echo.ErrInternalServerError.SetInternal(err)
	}

	reader, err := engine.Logs(&atom.EngineLogsRequest{ID: taskEntry.RuntimeID})
	if err != nil {
		return echo.ErrInternalServerError.SetInternal(err)
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			log.Error("close log reader", "error", closeErr)
		}
	}()

	res := c.Response()
	res.Header().Set(echo.HeaderContentType, "text/plain; charset=utf-8")
	res.WriteHeader(http.StatusOK)

	flusher, _ := res.Writer.(http.Flusher)
	buf := make([]byte, 4096)

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			n, readErr := reader.Read(buf)
			if n > 0 {
				if _, writeErr := res.Writer.Write(buf[:n]); writeErr != nil {
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
