package system

import (
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/service/system"
	"github.com/labstack/echo/v5"
)

// Nodes returns the cluster nodes and their usage.
func Nodes(c *echo.Context) error {
	svc := system.New(c.Request().Context())
	resp, err := svc.Nodes()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, resp)
}

// Features returns the enabled feature toggles.
func Features(c *echo.Context) error {
	svc := system.New(c.Request().Context())
	resp, err := svc.Features()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, resp)
}
