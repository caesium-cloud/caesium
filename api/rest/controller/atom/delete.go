package atom

import (
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/service/atom"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

func Delete(c echo.Context) error {
	id := uuid.MustParse(c.Param("id"))

	if err := atom.Service().Delete(id); err != nil {
		return echo.ErrInternalServerError.SetInternal(err)
	}

	return c.NoContent(http.StatusAccepted)
}
