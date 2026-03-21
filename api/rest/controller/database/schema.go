package database

import (
	"net/http"

	dbsvc "github.com/caesium-cloud/caesium/api/rest/service/database"
	"github.com/labstack/echo/v5"
)

func Schema(c *echo.Context) error {
	svc := dbsvc.New(c.Request().Context())
	resp, err := svc.Schema()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to inspect database schema").Wrap(err)
	}
	return c.JSON(http.StatusOK, resp)
}
