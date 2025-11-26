package run

import (
	"errors"
	"net/http"

	runsvc "github.com/caesium-cloud/caesium/api/rest/service/run"
	"github.com/caesium-cloud/caesium/internal/callback"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

// RetryCallbacks triggers retries for failed callbacks associated with the run.
func RetryCallbacks(c echo.Context) error {
	ctx := c.Request().Context()

	jobID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.ErrBadRequest.SetInternal(err)
	}
	runID, err := uuid.Parse(c.Param("run_id"))
	if err != nil {
		return echo.ErrBadRequest.SetInternal(err)
	}

	runEntry, err := runsvc.New(ctx).Get(runID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.ErrInternalServerError.SetInternal(err)
	}

	if runEntry.JobID != jobID {
		return echo.ErrNotFound
	}

	if err := callback.Default().RetryFailed(ctx, runID); err != nil {
		return echo.ErrInternalServerError.SetInternal(err)
	}

	updated, err := runsvc.New(ctx).Get(runID)
	if err != nil {
		return echo.ErrInternalServerError.SetInternal(err)
	}

	return c.JSON(http.StatusAccepted, updated)
}
