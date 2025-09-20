package job

import (
	"errors"
	"net/http"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	tsvc "github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

type JobResponse struct {
	*models.Job
	Trigger   *models.Trigger `json:"trigger,omitempty"`
	LatestRun *run.Run        `json:"latest_run,omitempty"`
}

func Get(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.ErrBadRequest.SetInternal(err)
	}

	ctx := c.Request().Context()

	j, err := jsvc.Service(ctx).Get(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}

		return echo.ErrInternalServerError.SetInternal(err)
	}

	resp := &JobResponse{Job: j}

	if trig, err := tsvc.Service(ctx).Get(j.TriggerID); err == nil {
		resp.Trigger = trig
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return echo.ErrInternalServerError.SetInternal(err)
	}

	resp.LatestRun = run.Default().Latest(j.ID)

	return c.JSON(http.StatusOK, resp)
}
