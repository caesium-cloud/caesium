package trigger

import (
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

func Get(c *echo.Context) error {
	id := uuid.MustParse(c.Param("id"))

	t, err := trigger.Service(c.Request().Context()).Get(id)

	switch {
	case err != nil:
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	case t == nil:
		return echo.ErrNotFound
	default:
		return c.JSON(http.StatusOK, t)
	}
}
