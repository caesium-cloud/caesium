package atom

import (
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/service/atom"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

func Get(c *echo.Context) error {
	id := uuid.MustParse(c.Param("id"))

	a, err := atom.Service(c.Request().Context()).Get(id)

	switch {
	case err != nil:
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	case a == nil:
		return echo.ErrNotFound
	default:
		return c.JSON(http.StatusOK, a)
	}
}
