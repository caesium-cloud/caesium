package lineage

import (
	"net/http"
	"strconv"

	lsvc "github.com/caesium-cloud/caesium/api/rest/service/lineage"
	"github.com/labstack/echo/v5"
)

// Impact handles GET /lineage/impact?namespace=<ns>&name=<name>[&max_depth=N]
//
// It returns the transitive set of downstream datasets that would be affected
// if the named dataset changed — i.e. "what breaks if this table changes."
// Results are breadth-first, shallowest hops first, and bounded by max_depth
// (default 10, capped at 20).
//
// Example:
//
//	GET /lineage/impact?namespace=default&name=analytics.fact_orders
func Impact(c *echo.Context) error {
	namespace := c.QueryParam("namespace")
	name := c.QueryParam("name")

	if namespace == "" || name == "" {
		return echo.NewHTTPError(http.StatusBadRequest,
			"namespace and name query parameters are required")
	}

	maxDepth := 0
	if raw := c.QueryParam("max_depth"); raw != "" {
		d, err := strconv.Atoi(raw)
		if err != nil || d < 0 {
			return echo.NewHTTPError(http.StatusBadRequest,
				"max_depth must be a non-negative integer")
		}
		maxDepth = d
	}

	ctx := c.Request().Context()

	result, err := lsvc.New(ctx).Impact(namespace, name, maxDepth)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	return c.JSON(http.StatusOK, result)
}
