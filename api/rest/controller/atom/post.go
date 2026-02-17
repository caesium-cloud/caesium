package atom

import (
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/service/atom"
	"github.com/labstack/echo/v5"
)

func Post(c *echo.Context) error {
	var req atom.CreateRequest

	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	a, err := atom.Service(c.Request().Context()).Create(&req)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	return c.JSON(http.StatusCreated, a)
}
