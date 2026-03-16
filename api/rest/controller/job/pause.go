package job

import (
	"errors"
	"net/http"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

func Pause(c *echo.Context) error {
	return setPaused(c, true)
}

func Unpause(c *echo.Context) error {
	return setPaused(c, false)
}

func setPaused(c *echo.Context, paused bool) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	j, err := jsvc.Service(c.Request().Context()).SetPaused(id, paused)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}

		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	return c.JSON(http.StatusOK, j)
}
