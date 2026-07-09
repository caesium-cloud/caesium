// Package reproduce exposes the read-only task execution descriptor endpoint.
package reproduce

import (
	"errors"
	"net/http"

	rsvc "github.com/caesium-cloud/caesium/api/rest/service/reproduce"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// Get handles GET /jobs/:id/runs/:run_id/tasks/:task/descriptor.
func Get(c *echo.Context) error {
	// Parse and validate BOTH path UUIDs up-front so the job/run ownership
	// cross-check cannot be bypassed with a malformed job id.
	jobID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}
	runID, err := uuid.Parse(c.Param("run_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	task := c.Param("task")
	if task == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request")
	}

	resp, err := rsvc.New(c.Request().Context()).Descriptor(runID, task)
	if err != nil {
		switch {
		case errors.Is(err, rsvc.ErrDescriptorUnavailable):
			return echo.NewHTTPError(http.StatusNotFound, "descriptor unavailable")
		case errors.Is(err, gorm.ErrRecordNotFound), errors.Is(err, runstorage.ErrTaskRunNotFound):
			return echo.ErrNotFound
		default:
			return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
		}
	}

	// Guard against addressing a run under a different job path.
	if resp.JobID != jobID {
		return echo.ErrNotFound
	}

	return c.JSON(http.StatusOK, resp)
}
