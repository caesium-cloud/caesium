package lineage

import (
	"errors"
	"net/http"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// RunDatasets handles GET /v1/jobs/:id/runs/:run_id/datasets: the lineage
// dataset identities (namespace, name, direction, observing step) persisted for
// a run, so an operator can enter the lineage impact view from a run.
//
// The list is empty (never 404) for a run that persisted no lineage, including
// every run while CAESIUM_OPEN_LINEAGE_ENABLED is off. The run is history: the
// route is gated like the other single-run reads (job's current namespace AND
// the run's persisted namespace), and the handler reads under the history
// scope.
func RunDatasets(c *echo.Context) error {
	ctx := authz.WithHistoryScope(c.Request().Context(), authmw.GetPrincipal(c))

	jobID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}
	runID, err := uuid.Parse(c.Param("run_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	if _, err = jsvc.Service(ctx).Get(jobID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	result, err := lineageService(ctx).RunDatasets(jobID, runID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, result)
}
