package cluster

import (
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/service/private/cluster"
	"github.com/caesium-cloud/caesium/db/store"
	"github.com/labstack/echo/v4"
)

func Join(c echo.Context) error {
	var req *cluster.JoinRequest

	if err := c.Bind(req); err != nil {
		return c.JSON(http.StatusBadRequest, err)
	}

	if req.ID == "" {
		return c.JSON(http.StatusBadRequest, "id must not be empty")
	}

	if req.Address == "" {
		return c.JSON(http.StatusBadRequest, "address must to be empty")
	}

	err := cluster.Service().Join(req)

	switch err {
	case nil:
		return c.JSON(http.StatusNoContent, nil)
	case store.ErrNotLeader:
		return Redirect(c)
	default:
		return c.JSON(http.StatusInternalServerError, err)
	}
}
