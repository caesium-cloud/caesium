package stats

import (
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/service/stats"
	"github.com/labstack/echo/v5"
)

// Get returns aggregated job statistics.
func Get(c *echo.Context) error {
	svc := stats.New(c.Request().Context())
	resp, err := svc.Get()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, resp)
}
