package db

import (
	"net/http"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/labstack/echo/v4"
)

func Migrate(c echo.Context) error {
	database := db.Connection()

	for _, mod := range []interface{}{
		&models.Atom{},
		&models.Callback{},
		&models.Job{},
		&models.Task{},
		&models.Trigger{},
	} {
		if err := database.AutoMigrate(mod); err != nil {
			return echo.ErrInternalServerError.SetInternal(err)
		}
	}

	return c.JSON(http.StatusNoContent, nil)
}
