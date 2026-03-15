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

// PostRequest holds the optional body for POST /v1/jobs/:id/run.
type PostRequest struct {
	Params map[string]string `json:"params,omitempty"`
}

func Post(c *echo.Context) error {
	ctx := c.Request().Context()

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	// Parse optional request body — ignore decode errors so an empty body is fine.
	var req PostRequest
	_ = c.Bind(&req)

	j, err := jsvc.Service(ctx).Get(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}

		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	r, err := runsvc.New(ctx).Start(j.ID, nil, req.Params)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	go func() {
		runCtx := runstorage.WithContext(context.Background(), r.ID)
		if err := job.New(j, job.WithTriggerID(nil), job.WithParams(r.Params)).Run(runCtx); err != nil {
			log.Error("job run failure", "id", j.ID, "run_id", r.ID, "error", err)
		}
	}()

	return c.JSON(http.StatusAccepted, r)
}
