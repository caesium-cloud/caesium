package lineage

import (
	"net/http"
	"strconv"

	lsvc "github.com/caesium-cloud/caesium/api/rest/service/lineage"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/labstack/echo/v5"
)

// lineageService builds the service a request reads; handler tests swap it
// for one over a test database.
var lineageService = lsvc.New

// Impact handles GET /lineage/impact?namespace=<ns>&name=<name>[&max_depth=N]
//
// It returns the transitive set of downstream datasets that would be affected
// if the named dataset changed — i.e. "what breaks if this table changes."
// Results are breadth-first, shallowest hops first.
//
// max_depth semantics:
//   - omitted or 0: use the server default (10 hops).
//   - 1–20: traverse exactly that many hops.
//   - >20: capped at 20 by the query layer.
//   - negative: rejected with 400.
//
// Example:
//
//	GET /lineage/impact?namespace=default&name=analytics.fact_orders
//	GET /lineage/impact?namespace=default&name=analytics.fact_orders&max_depth=3
func Impact(c *echo.Context) error {
	namespace := c.QueryParam("namespace")
	name := c.QueryParam("name")

	// An explicit empty namespace is the declared registry's v1 identity.
	// Missing namespace remains invalid; silently defaulting it would query a
	// different graph from the identity the caller supplied.
	if !c.QueryParams().Has("namespace") || name == "" {
		return echo.NewHTTPError(http.StatusBadRequest,
			"namespace and name query parameters are required")
	}

	// maxDepth=0 is passed through to QueryImpact, which treats it as
	// "use the server default of 10 hops."  Negative values are invalid.
	maxDepth := 0
	if raw := c.QueryParam("max_depth"); raw != "" {
		d, err := strconv.Atoi(raw)
		if err != nil || d < 0 {
			return echo.NewHTTPError(http.StatusBadRequest,
				"max_depth must be a non-negative integer (0 = server default)")
		}
		maxDepth = d
	}

	ctx := c.Request().Context()

	// A namespace-confined caller gets the whole-graph answer redacted at the
	// boundary (identity-and-access B5); everyone else gets it verbatim.
	if redactor := authz.NewRedactor(ctx); redactor != nil {
		redacted, err := lineageService(ctx).ConfinedImpact(redactor, namespace, name, maxDepth)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
		}
		redactor.Done()
		return c.JSON(http.StatusOK, redacted)
	}

	result, err := lineageService(ctx).Impact(namespace, name, maxDepth)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	return c.JSON(http.StatusOK, result)
}
