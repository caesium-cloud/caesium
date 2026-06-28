package trigger

import (
	"net/http"

	eventsvc "github.com/caesium-cloud/caesium/api/rest/service/event"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

func Events(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	req, err := eventsvc.ListRequestFromValues(c.QueryParams())
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	events, err := eventsvc.New(c.Request().Context()).ListTriggerEvents(id, req)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, events)
}
