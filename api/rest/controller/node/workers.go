package node

import (
	"net/http"
	"strings"

	workersvc "github.com/caesium-cloud/caesium/api/rest/service/worker"
	"github.com/labstack/echo/v5"
)

// Workers returns distributed worker status for a given node address.
func Workers(c *echo.Context) error {
	address := strings.TrimSpace(c.Param("address"))
	if address == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "address is required")
	}

	status, err := workersvc.New(c.Request().Context()).Status(address)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, status)
}
