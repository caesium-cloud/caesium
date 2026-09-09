package dataset

import (
	"errors"
	"net/http"
	"strings"

	svc "github.com/caesium-cloud/caesium/api/rest/service/dataset"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// Holds handles GET /v1/datasets/holds.
func (ctrl *Controller) Holds(c *echo.Context) error {
	params := svc.HoldsParams{Status: c.QueryParam("status"), Name: c.QueryParam("name")}
	if c.QueryParams().Has("namespace") {
		namespace := svc.NamespaceFromPath(c.QueryParam("namespace"))
		params.Namespace = &namespace
	}
	if err := parsePagination(c, &params.Limit, &params.Offset); err != nil {
		return err
	}
	result, err := svc.New(c.Request().Context()).Holds(params)
	if err != nil {
		if errors.Is(err, svc.ErrHoldStatus) {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, result)
}

// Metrics handles GET /v1/datasets/:ns/:name/metrics?metric=rowCount.
func (ctrl *Controller) Metrics(c *echo.Context) error {
	namespace, name := datasetPath(c)
	if name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "dataset name is required")
	}
	params := svc.MetricsParams{}
	if err := parsePagination(c, &params.Limit, &params.Offset); err != nil {
		return err
	}
	result, err := svc.New(c.Request().Context()).Metrics(namespace, name, strings.TrimSpace(c.QueryParam("metric")), params)
	if err != nil {
		if errors.Is(err, svc.ErrMetricRequired) {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, result)
}
