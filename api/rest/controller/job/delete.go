package job

import (
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

func Delete(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.ErrBadRequest.SetInternal(err)
	}

	if err := job.Service(c.Request().Context()).Delete(id); err != nil {
		return echo.ErrInternalServerError.SetInternal(err)
	}

	return c.NoContent(http.StatusAccepted)
}
