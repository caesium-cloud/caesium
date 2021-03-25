package db

import (
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/controller/private/cluster"
	"github.com/caesium-cloud/caesium/api/rest/service/private/db"
	"github.com/caesium-cloud/caesium/db/store"
	"github.com/labstack/echo/v4"
)

func Query(c echo.Context) error {
	var req *db.QueryRequest

	if err := c.Bind(req); err != nil {
		return c.JSON(http.StatusBadRequest, err)
	}

	svc := db.Service()
	resp, err := svc.Query(req)

	switch err {
	case nil:
		return c.JSON(http.StatusOK, resp)
	case store.ErrNotLeader:
		return cluster.Redirect(c)
	default:
		return c.JSON(http.StatusInternalServerError, err)
	}
}