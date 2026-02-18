package trigger

import (
	"context"
	"fmt"
	codes "net/http"

	"github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/caesium-cloud/caesium/internal/executor"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/trigger/http"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

func Put(c *echo.Context) error {
	var (
		ctx = context.Background()
		id  = uuid.MustParse(c.Param("id"))
		svc = trigger.Service(ctx)
	)

	t, err := svc.Get(id)
	switch {
	case err != nil:
		return echo.NewHTTPError(codes.StatusInternalServerError, "internal server error").Wrap(err)
	case t == nil:
		return echo.ErrNotFound
	case t.Type != models.TriggerTypeHTTP:
		return echo.NewHTTPError(codes.StatusBadRequest, "bad request").Wrap(
			fmt.Errorf(
				"trigger: '%v' is type: '%v', not '%v'",
				t.ID,
				t.Type,
				models.TriggerTypeHTTP,
			),
		)
	default:
		h, err := http.New(t)
		if err != nil {
			return echo.NewHTTPError(codes.StatusInternalServerError, "internal server error").Wrap(err)
		}

		executor.Queue(ctx, h)

		return c.JSON(codes.StatusAccepted, nil)
	}
}
