package db

import (
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/controller/private/cluster"
	"github.com/caesium-cloud/caesium/api/rest/service/private/db"
	"github.com/caesium-cloud/caesium/db/store"
	"github.com/labstack/echo/v4"
)

func Execute(c echo.Context) error {
	var req db.ExecuteRequest

	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, err)
	}

	svc := db.Service()
	resp, err := svc.Execute(&req)

	switch err {
	case nil:
		return c.JSON(http.StatusOK, resp)
	case store.ErrNotLeader:
		return cluster.Redirect(c)
	default:
		return c.JSON(http.StatusInternalServerError, err)
	}
}
