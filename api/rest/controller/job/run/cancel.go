package run

import (
	"context"
	"errors"
	"net/http"
	"strings"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	runsvc "github.com/caesium-cloud/caesium/api/rest/service/run"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// errRunCancelNotFound hides a run whose namespace the caller cannot read.
// The response is 404, the same answer a run that is not this job's gets.
var errRunCancelNotFound = errors.New("run not found")

// CancelConflictResponse is the 409 body when the run is not running.
type CancelConflictResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Status  string `json:"status,omitempty"`
}

// Cancel stops one running run of a job. It uses the same cancel transaction
// as concurrency replacement (CancelRunningRun → cancelRunTx). A run that is
// already terminal is a conflict, not a second cancel.
func Cancel(c *echo.Context) error {
	ctx := c.Request().Context()

	jobID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}
	runID, err := uuid.Parse(c.Param("run_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	if _, err = cancelGetJob(ctx, jobID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	runEntry, err := cancelGetRun(ctx, runID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	if runEntry == nil || runEntry.JobID != jobID {
		return echo.ErrNotFound
	}

	// The route authorised operator in the job's current namespace and, for a
	// confined caller, in the run's persisted namespace. Re-check the persisted
	// namespace so a cluster principal, who skips the resolver, still cannot
	// cancel a run they cannot operate, and a run they cannot read stays 404.
	if err := authorizeRunCancel(authmw.GetPrincipal(c), runEntry.Namespace); err != nil {
		if _, ok := errors.AsType[*authz.Denial](err); ok {
			return err
		}
		return echo.ErrNotFound
	}

	if runEntry.Status != runstorage.StatusRunning {
		return writeRunNotRunning(c, runEntry.Status)
	}
	if err := cancelRunningRun(ctx, runID); err != nil {
		if errors.Is(err, runstorage.ErrRunNotRunning) {
			status := runEntry.Status
			if fresh, getErr := cancelGetRun(ctx, runID); getErr == nil && fresh != nil {
				status = fresh.Status
			}
			return writeRunNotRunning(c, status)
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	cancelled, err := cancelGetRun(ctx, runID)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, cancelled)
}

func writeRunNotRunning(c *echo.Context, status runstorage.Status) error {
	return c.JSON(http.StatusConflict, CancelConflictResponse{
		Error:   "run_not_running",
		Message: "only a running run can be cancelled",
		Status:  string(status),
	})
}

// authorizeRunCancel decides whether p may cancel a run recorded in namespace.
// A nil principal (auth disabled) and one holding operator cluster-wide always
// may. Otherwise a namespace p may not read is not found, and a namespace p
// may read but not operate on is a 403 denial.
func authorizeRunCancel(p *auth.Principal, namespace string) error {
	if p == nil || auth.HasRole(p.ClusterRole(), models.RoleOperator) {
		return nil
	}
	owner := models.NamespaceOrDefault(strings.TrimSpace(namespace))
	if !auth.HasRole(p.RoleIn(owner), models.RoleViewer) {
		return errRunCancelNotFound
	}
	return authz.RequireRoleIn(p, models.RoleOperator, owner)
}

var (
	cancelGetJob = func(ctx context.Context, id uuid.UUID) (*models.Job, error) {
		return jsvc.Service(ctx).Get(id)
	}
	cancelGetRun = func(ctx context.Context, id uuid.UUID) (*runstorage.JobRun, error) {
		return runsvc.New(ctx).Get(id)
	}
	cancelRunningRun = func(ctx context.Context, id uuid.UUID) error {
		return runstorage.Default().CancelRunningRun(ctx, id)
	}
)
