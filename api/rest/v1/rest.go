package rest

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// Bind the REST endpoints to the versioned endpoint group.
func Bind(group *echo.Group) {
	group.GET("/placeholder", func(c echo.Context) error {
		return c.JSON(http.StatusNoContent, nil)
	})
}
