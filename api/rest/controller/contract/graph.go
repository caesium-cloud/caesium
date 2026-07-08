package contract

import (
	"errors"
	"net/http"

	svc "github.com/caesium-cloud/caesium/api/rest/service/contract"
	"github.com/labstack/echo/v5"
)

func Graph(c *echo.Context) error {
	graph, err := svc.New(c.Request().Context()).Graph(c.QueryParam("dataset"), nil)
	if err != nil {
		switch {
		case errors.Is(err, svc.ErrDisabled):
			return echo.ErrNotFound
		case errors.Is(err, svc.ErrInvalidDatasetFilter):
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		default:
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to derive contract graph").Wrap(err)
		}
	}
	svc.RecordFindings(*graph)
	return c.JSON(http.StatusOK, graph)
}
