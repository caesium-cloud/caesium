// Package reproduce exposes task execution descriptor endpoints.
package reproduce

import (
	"errors"
	"net/http"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	rsvc "github.com/caesium-cloud/caesium/api/rest/service/reproduce"
	"github.com/caesium-cloud/caesium/internal/authz"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// Get handles GET /jobs/:id/runs/:run_id/tasks/:task/descriptor.
func Get(c *echo.Context) error {
	return handleDescriptor(c, false)
}

// Prune handles DELETE on the descriptor resource.
func Prune(c *echo.Context) error {
	if authmw.GetPrincipal(c) == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "authentication required")
	}
	return handleDescriptor(c, true)
}

func handleDescriptor(c *echo.Context, prune bool) error {
	// Parse and validate BOTH path UUIDs up-front so the job/run ownership
	// cross-check cannot be bypassed with a malformed job id.
	jobID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}
	runID, err := uuid.Parse(c.Param("run_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	task := c.Param("task")
	if task == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request")
	}

	// The prior-run blob the mini-why diffs against is history: a moved job's
	// old runs stay with their old owner, same as caesium why.
	ctx := authz.WithHistoryScope(c.Request().Context(), authmw.GetPrincipal(c))
	svc := rsvc.New(ctx).ForJob(jobID)
	var resp *rsvc.DescriptorResponse
	if prune {
		err = svc.Prune(jobID, runID, task, authmw.GetPrincipal(c))
	} else {
		resp, err = svc.Descriptor(runID, task)
	}
	if err != nil {
		if _, ok := errors.AsType[*authz.Denial](err); ok {
			return err
		}
		switch {
		case errors.Is(err, rsvc.ErrDescriptorPruned):
			return echo.NewHTTPError(http.StatusNotFound, "descriptor pruned")
		case errors.Is(err, rsvc.ErrRunActive):
			return echo.NewHTTPError(http.StatusConflict, err.Error())
		case errors.Is(err, rsvc.ErrDescriptorUnavailable):
			return echo.NewHTTPError(http.StatusNotFound, "descriptor unavailable")
		case errors.Is(err, rsvc.ErrFannedTaskAmbiguous):
			// Fail closed on a fanned baseline rather than reproducing an
			// arbitrary partition's container.
			return echo.NewHTTPError(http.StatusConflict, "task is a fan-out group; descriptors are per-partition")
		case errors.Is(err, gorm.ErrRecordNotFound), errors.Is(err, runstorage.ErrTaskRunNotFound):
			return echo.ErrNotFound
		default:
			return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
		}
	}

	if prune {
		return c.NoContent(http.StatusNoContent)
	}
	return c.JSON(http.StatusOK, resp)
}
