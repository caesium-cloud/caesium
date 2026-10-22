package contract

import (
	"errors"
	"net/http"

	svc "github.com/caesium-cloud/caesium/api/rest/service/contract"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/labstack/echo/v5"
)

// contractService builds the service a request reads; handler tests swap it
// for one over a test database.
var contractService = svc.New

func Graph(c *echo.Context) error {
	ctx := c.Request().Context()
	// A namespace-confined caller gets the whole graph redacted at the
	// boundary (identity-and-access B5); everyone else gets it verbatim.
	if redactor := authz.NewRedactor(ctx); redactor != nil {
		graph, recorded, err := contractService(ctx).ConfinedGraph(redactor, c.QueryParam("dataset"))
		if err != nil {
			return graphError(err)
		}
		svc.RecordFindings(*recorded)
		redactor.Done()
		return c.JSON(http.StatusOK, graph)
	}
	graph, err := contractService(ctx).Graph(c.QueryParam("dataset"), nil)
	if err != nil {
		return graphError(err)
	}
	svc.RecordFindings(*graph)
	return c.JSON(http.StatusOK, graph)
}

func graphError(err error) error {
	switch {
	case errors.Is(err, svc.ErrDisabled):
		return echo.ErrNotFound
	case errors.Is(err, svc.ErrInvalidDatasetFilter):
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	default:
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to derive contract graph").Wrap(err)
	}
}
