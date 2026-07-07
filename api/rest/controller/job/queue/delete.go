package queue

import (
	"errors"
	"net/http"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

func Delete(c *echo.Context) error {
	ctx := c.Request().Context()

	jobID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}
	queueID, err := uuid.Parse(c.Param("queue_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	svc := jsvc.Service(ctx)
	if _, err = svc.Get(jobID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	if err := svc.CancelQueuedRun(jobID, queueID); err != nil {
		if errors.Is(err, runstorage.ErrQueuedRunNotFound) {
			return echo.NewHTTPError(http.StatusNotFound, "queued run not found")
		}
		if errors.Is(err, runstorage.ErrQueuedRunUnavailable) {
			return echo.NewHTTPError(http.StatusConflict, "queued run already started")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	return c.NoContent(http.StatusNoContent)
}
