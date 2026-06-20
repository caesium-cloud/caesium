// Package why implements the causal-explainer endpoint (data-plane-memory A3):
//
//	GET /v1/jobs/:id/runs/:run_id/why?task=<task-name-or-id>
//
// It returns a field-by-field explanation of why the given task in the run
// executed, hit the cache, or re-ran — diffing the persisted HashInput blobs and
// joining trigger-side causation from the event store.
package why

import (
	"errors"
	"net/http"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	whysvc "github.com/caesium-cloud/caesium/api/rest/service/why"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// Get handles GET /v1/jobs/:id/runs/:run_id/why?task=<task>.
func Get(c *echo.Context) error {
	ctx := c.Request().Context()

	jobID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	runID, err := uuid.Parse(c.Param("run_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	task := c.QueryParam("task")
	if task == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "task query parameter is required")
	}

	// Verify the job exists before touching the run.
	if _, err = jsvc.Service(ctx).Get(jobID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	explanation, err := whysvc.New(ctx).Why(runID, task)
	if err != nil {
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound), errors.Is(err, runstorage.ErrTaskRunNotFound):
			return echo.ErrNotFound
		default:
			return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
		}
	}

	// Guard against a run from a different job being addressed under this job's
	// path.
	if explanation.JobID != jobID {
		return echo.ErrNotFound
	}

	return c.JSON(http.StatusOK, explanation)
}
