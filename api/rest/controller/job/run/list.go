package run

import (
	"errors"
	"net/http"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	runstore "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

func List(c echo.Context) error {
	ctx := c.Request().Context()

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.ErrBadRequest.SetInternal(err)
	}

	if _, err = jsvc.Service(ctx).Get(id); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}

		return echo.ErrInternalServerError.SetInternal(err)
	}

	runs := runstore.Default().List(id)

	return c.JSON(http.StatusOK, runs)
}
