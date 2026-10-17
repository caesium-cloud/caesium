package trigger

import (
	"net/http"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	eventsvc "github.com/caesium-cloud/caesium/api/rest/service/event"
	"github.com/caesium-cloud/caesium/internal/authz"
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

	// The trigger is authorised on every namespace it serves today; its event
	// history is filtered by each delivery's own runs (B4).
	ctx := authz.WithHistoryScope(c.Request().Context(), authmw.GetPrincipal(c))
	events, err := eventsvc.New(ctx).ListTriggerEvents(id, req)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, events)
}
