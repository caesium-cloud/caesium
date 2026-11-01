package trigger

import (
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// triggerGetService builds the service Get reads; handler tests swap it for
// one over a test database.
var triggerGetService = trigger.Service

func Get(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	t, err := triggerGetService(c.Request().Context()).Get(id)
	if err != nil {
		return triggerServiceError(err)
	}
	return c.JSON(http.StatusOK, t)
}
