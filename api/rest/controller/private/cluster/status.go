package cluster

import (
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/service/private/cluster"
	"github.com/labstack/echo/v4"
)

func Status(c echo.Context) error {
	status, err := cluster.Service().Status()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, err)
	}

	return c.JSON(http.StatusOK, status)
}
