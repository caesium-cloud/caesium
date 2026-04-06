package trigger

import (
	"errors"
	"net/http"

	triggersvc "github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

func Patch(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	req := &triggersvc.UpdateRequest{}
	if err := c.Bind(req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	trigger, err := triggerServiceFactory(c.Request().Context()).Update(id, req)
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return echo.ErrNotFound
	case err != nil:
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	default:
		return c.JSON(http.StatusOK, trigger)
	}
}
