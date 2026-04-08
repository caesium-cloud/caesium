package auth

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

func ListKeys(c *echo.Context) error {
	keys, err := Dependencies.Service.ListKeys()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to list api keys").Wrap(err)
	}
	return c.JSON(http.StatusOK, keys)
}
