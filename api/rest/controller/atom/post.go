package atom

import (
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/service/atom"
	"github.com/labstack/echo/v4"
)

func Post(c echo.Context) error {
	var req atom.CreateRequest

	if err := c.Bind(&req); err != nil {
		return echo.ErrBadRequest.SetInternal(err)
	}

	a, err := atom.Service().Create(&req)
	if err != nil {
		return echo.ErrInternalServerError.SetInternal(err)
	}

	return c.JSON(http.StatusCreated, a)
}
