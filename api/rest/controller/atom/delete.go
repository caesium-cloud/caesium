package atom

import (
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/service/atom"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

func Delete(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	if err := atom.Service(c.Request().Context()).Delete(id); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	return c.NoContent(http.StatusAccepted)
}
