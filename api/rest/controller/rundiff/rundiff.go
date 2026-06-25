// Package rundiff implements the run-diff endpoint:
//
//	GET /v1/jobs/:id/runs/diff?left=<run>&right=<run>
package rundiff

import (
	"errors"
	"net/http"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	runsvc "github.com/caesium-cloud/caesium/api/rest/service/run"
	diffsvc "github.com/caesium-cloud/caesium/api/rest/service/rundiff"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// Get handles GET /v1/jobs/:id/runs/diff?left=<run>&right=<run>.
func Get(c *echo.Context) error {
	ctx := c.Request().Context()

	jobID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	leftRaw := c.QueryParam("left")
	rightRaw := c.QueryParam("right")
	if leftRaw == "" || rightRaw == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "left and right query parameters are required")
	}

	leftRunID, err := uuid.Parse(leftRaw)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	rightRunID, err := uuid.Parse(rightRaw)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	if _, err = jsvc.Service(ctx).Get(jobID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	runService := runsvc.New(ctx)
	leftRun, err := runService.Get(leftRunID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	if leftRun.JobID != jobID {
		return echo.ErrNotFound
	}

	rightRun, err := runService.Get(rightRunID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	if rightRun.JobID != jobID {
		return echo.ErrNotFound
	}

	diff, err := diffsvc.New(ctx).Diff(jobID, leftRunID, rightRunID)
	if err != nil {
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound), errors.Is(err, runstorage.ErrRunDiffJobMismatch):
			return echo.ErrNotFound
		default:
			return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
		}
	}

	return c.JSON(http.StatusOK, diff)
}
