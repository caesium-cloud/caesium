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

func Post(c *echo.Context) error {
	ctx := c.Request().Context()

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	j, err := jsvc.Service(ctx).Get(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}

		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	r, err := runsvc.New(ctx).Start(j.ID)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	go func() {
		runCtx := runstorage.WithContext(context.Background(), r.ID)
		if err := job.New(j).Run(runCtx); err != nil {
			log.Error("job run failure", "id", j.ID, "run_id", r.ID, "error", err)
		}
	}()

	return c.JSON(http.StatusAccepted, r)
}
