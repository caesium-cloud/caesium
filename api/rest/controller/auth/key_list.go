package auth

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

func (ctrl *Controller) ListKeys(c *echo.Context) error {
	keys, err := ctrl.service.ListKeys()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to list api keys").Wrap(err)
	}
	return c.JSON(http.StatusOK, keys)
}
