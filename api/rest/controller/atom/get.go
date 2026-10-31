package atom

import (
	"errors"
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/service/atom"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// atomGetService builds the service Get reads; handler tests swap it for one
// over a test database.
var atomGetService = atom.Service

func Get(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	a, err := atomGetService(c.Request().Context()).Get(id)

	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return echo.ErrNotFound
	case err != nil:
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	default:
		return c.JSON(http.StatusOK, a)
	}
}
