package job

import (
	"errors"
	"net/http"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	runsvc "github.com/caesium-cloud/caesium/api/rest/service/run"
	tsvc "github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

type JobResponse struct {
	*models.Job
	Trigger   *models.Trigger    `json:"trigger,omitempty"`
	LatestRun *runstorage.JobRun `json:"latest_run,omitempty"`
}

func Get(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	ctx := c.Request().Context()

	j, err := jsvc.Service(ctx).Get(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}

		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	resp := &JobResponse{Job: j}

	if trig, err := tsvc.Service(ctx).Get(j.TriggerID); err == nil {
		resp.Trigger = trig
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	latest, err := runsvc.New(ctx).Latest(j.ID)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	resp.LatestRun = latest

	return c.JSON(http.StatusOK, resp)
}
