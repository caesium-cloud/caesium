package queue

import (
	"errors"
	"net/http"
	"strings"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

func Delete(c *echo.Context) error {
	ctx := c.Request().Context()

	jobID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}
	queueID, err := uuid.Parse(c.Param("queue_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	svc := jsvc.Service(ctx)
	if _, err = svc.Get(jobID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	// The route authorised operator in the job's CURRENT namespace. A queued
	// request belongs to the namespace it was queued in (identity-and-access
	// D1), so cancelling it also needs operator there.
	principal := authmw.GetPrincipal(c)
	cancelSvc := jsvc.Service(jsvc.WithQueuedRunCancelCheck(ctx, func(namespace *string) error {
		return authorizeQueuedRunCancel(principal, namespace)
	}))
	if err := cancelSvc.CancelQueuedRun(jobID, queueID); err != nil {
		if _, ok := errors.AsType[*authz.Denial](err); ok {
			// The auth middleware answers 403 and audits the denial.
			return err
		}
		if errors.Is(err, runstorage.ErrQueuedRunNotFound) {
			return echo.NewHTTPError(http.StatusNotFound, "queued run not found")
		}
		if errors.Is(err, runstorage.ErrQueuedRunUnavailable) {
			return echo.NewHTTPError(http.StatusConflict, "queued run already started")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	return c.NoContent(http.StatusNoContent)
}

// authorizeQueuedRunCancel decides whether p may cancel a queued request
// recorded in namespace (nil for a row queued before run_queue.namespace
// existed). A nil principal (auth disabled) and one holding operator
// cluster-wide always may, exactly as before. Otherwise:
//
//   - a row in a namespace p may not even read answers not found, the same
//     404 GET /v1/jobs/:id/queue gives by omitting it;
//   - a row p may read but not operate on is a 403 denial;
//   - a pre-upgrade row has no known owner: it is hidden from a confined
//     reader, and otherwise left to the route's check, as before.
func authorizeQueuedRunCancel(p *auth.Principal, namespace *string) error {
	if p == nil || auth.HasRole(p.ClusterRole(), models.RoleOperator) {
		return nil
	}
	if namespace == nil {
		if p.AllowedNamespaces() != nil {
			return runstorage.ErrQueuedRunNotFound
		}
		return nil
	}
	owner := models.NamespaceOrDefault(strings.TrimSpace(*namespace))
	if !auth.HasRole(p.RoleIn(owner), models.RoleViewer) {
		return runstorage.ErrQueuedRunNotFound
	}
	return authz.RequireRoleIn(p, models.RoleOperator, owner)
}
