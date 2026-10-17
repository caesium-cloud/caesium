package stats

import (
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/service/stats"
	"github.com/labstack/echo/v5"
)

// Get returns aggregated job statistics (legacy).
func Get(c *echo.Context) error {
	svc := statsService(c.Request().Context())
	resp, err := svc.Get()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, resp)
}

// Summary returns aggregated job statistics for a given window.
func Summary(c *echo.Context) error {
	window := c.QueryParam("window")
	if window == "" {
		window = "7d"
	}
	svc := statsService(c.Request().Context())
	resp, err := svc.Summary(window)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, resp)
}

// statsService builds the stats service for a request; a seam for the
// collection-guard tests, which serve the real handlers over a test database.
var statsService = stats.New
