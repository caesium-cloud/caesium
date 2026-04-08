package trigger

import (
	"net/http"

	triggersvc "github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/labstack/echo/v5"
)

func Post(c *echo.Context) error {
	req := &triggersvc.CreateRequest{}
	if err := c.Bind(req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	trigger, err := triggerServiceFactory(c.Request().Context()).Create(req)
	if err != nil {
		return triggerServiceError(err)
	}

	return c.JSON(http.StatusCreated, trigger)
}
