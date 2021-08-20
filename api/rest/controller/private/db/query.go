package db

import (
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/controller/private/cluster"
	"github.com/caesium-cloud/caesium/api/rest/service/private/db"
	"github.com/caesium-cloud/caesium/db/store"
	"github.com/labstack/echo/v4"
)

func Query(c echo.Context) error {
	var req db.QueryRequest

	if err := c.Bind(&req); err != nil {
		return echo.ErrBadRequest.SetInternal(err)
	}

	resp, err := db.Service().Query(&req)

	switch err {
	case nil:
		return c.JSON(http.StatusOK, resp)
	case store.ErrNotLeader:
		return cluster.Redirect(c)
	default:
		return echo.ErrInternalServerError.SetInternal(err)
	}
}
