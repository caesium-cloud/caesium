package run

import (
	"errors"
	"net/http"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	runstore "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

func Get(c echo.Context) error {
	ctx := c.Request().Context()

	jobID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.ErrBadRequest.SetInternal(err)
	}

	runID, err := uuid.Parse(c.Param("run_id"))
	if err != nil {
		return echo.ErrBadRequest.SetInternal(err)
	}

	if _, err = jsvc.Service(ctx).Get(jobID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}

		return echo.ErrInternalServerError.SetInternal(err)
	}

	runEntry, ok := runstore.Default().Get(runID)
	if !ok || runEntry.JobID != jobID {
		return echo.ErrNotFound
	}

	return c.JSON(http.StatusOK, runEntry)
}
