package db

import (
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/controller/private/cluster"
	"github.com/caesium-cloud/caesium/api/rest/service/private/db"
	"github.com/caesium-cloud/caesium/db/store"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/labstack/echo/v4"
)

func Migrate(c echo.Context) error {
	resp, err := db.Service().Query(&db.QueryRequest{
		Queries: []string{
			models.AtomCreate,
			models.TriggerCreate,
			models.TaskCreate,
			models.JobCreate,
			models.CallbackCreate,
		},
	})

	switch err {
	case nil:
		return c.JSON(http.StatusOK, resp)
	case store.ErrNotLeader:
		return cluster.Redirect(c)
	default:
		return echo.ErrInternalServerError.SetInternal(err)
	}
}
