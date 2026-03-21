package database

import (
	"errors"
	"net/http"

	dbsvc "github.com/caesium-cloud/caesium/api/rest/service/database"
	"github.com/labstack/echo/v5"
)

func Query(c *echo.Context) error {
	var req dbsvc.QueryRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid query payload").Wrap(err)
	}

	svc := dbsvc.New(c.Request().Context())
	resp, err := svc.Query(req)
	if err != nil {
		switch {
		case errors.Is(err, dbsvc.ErrEmptyQuery),
			errors.Is(err, dbsvc.ErrMultipleStatements),
			errors.Is(err, dbsvc.ErrUnsafeQuery):
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		default:
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
	}

	return c.JSON(http.StatusOK, resp)
}
