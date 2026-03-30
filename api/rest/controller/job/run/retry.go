package run

import (
	"context"
	"errors"
	"net/http"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	runsvc "github.com/caesium-cloud/caesium/api/rest/service/run"
	"github.com/caesium-cloud/caesium/internal/job"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// Retry retries a failed run, preserving cached/succeeded task results and
// re-executing only failed/skipped/pending tasks.
func Retry(c *echo.Context) error {
	ctx := c.Request().Context()

	jobID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}
	runID, err := uuid.Parse(c.Param("run_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	j, err := jsvc.Service(ctx).Get(jobID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	runEntry, err := runsvc.New(ctx).Get(runID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	if runEntry.JobID != jobID {
		return echo.ErrNotFound
	}

	store := runstorage.Default()
	r, err := store.RetryFromFailure(runID)
	if err != nil {
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	}

	go func() {
		runCtx := runstorage.WithContext(context.Background(), r.ID)
		if err := job.New(j, job.WithTriggerID(nil), job.WithParams(r.Params)).Run(runCtx); err != nil {
			log.Error("job retry run failure", "id", j.ID, "run_id", r.ID, "error", err)
		}
	}()

	return c.JSON(http.StatusAccepted, r)
}
