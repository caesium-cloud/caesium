package run

import (
	"context"
	"errors"
	"net/http"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/caesium-cloud/caesium/internal/authz"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// runAssertionsLoader reads a run's recorded assertion outcomes. It is a package
// var for the same reason the log loaders are: the handler's contract can be
// unit-tested without the process-wide default store.
var runAssertionsLoader = func(ctx context.Context, jobID, runID uuid.UUID) (*runstorage.RunAssertions, error) {
	return runstorage.Default().RunAssertions(ctx, jobID, runID)
}

// Assertions handles GET /v1/jobs/:id/runs/:run_id/assertions: the outcome of
// every declared data-assertion check of every task instance of the run — the
// ones that held as well as the ones that broke — as the evaluator decided at
// the time.
//
// The route is mounted only while CAESIUM_DATA_ASSERTIONS_ENABLED is set. The
// run is history: a namespace-confined reader gets it under the history scope,
// with baseline evidence not computed over the run's own namespace withheld, as
// the run read does for violations.
func Assertions(c *echo.Context) error {
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

	assertions, err := runAssertionsLoader(ctx, jobID, runID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, assertions)
}
