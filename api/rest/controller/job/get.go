package job

import (
	"context"
	"errors"
	"net/http"

	"github.com/caesium-cloud/caesium/api/middleware"
	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	runsvc "github.com/caesium-cloud/caesium/api/rest/service/run"
	tsvc "github.com/caesium-cloud/caesium/api/rest/service/trigger"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

type JobResponse struct {
	*models.Job
	Trigger   *models.Trigger    `json:"trigger,omitempty"`
	LatestRun *runstorage.JobRun `json:"latest_run,omitempty"`
}

func Get(c *echo.Context) error {
	// The job is authorised on its current namespace; its latest run is
	// history and is limited to the run namespaces the caller may read (B4).
	ctx := authz.WithHistoryScope(c.Request().Context(), middleware.GetPrincipal(c))

	j, err := jsvc.Service(ctx).GetByIDPrefix(c.Param("id"))
	if err != nil {
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			return echo.ErrNotFound
		case errors.Is(err, jsvc.ErrInvalidJobIDPrefix):
			return echo.NewHTTPError(http.StatusBadRequest, err.Error()).Wrap(err)
		case errors.Is(err, jsvc.ErrAmbiguousJobIDPrefix):
			return echo.NewHTTPError(http.StatusConflict, err.Error()).Wrap(err)
		}

		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	resp := &JobResponse{Job: j}

	if trig, err := tsvc.Service(ctx).Get(j.TriggerID); err == nil {
		visible, err := embeddedTriggerVisible(ctx, j.TriggerID)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
		}
		if visible {
			resp.Trigger = trig
		}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	latest, err := runsvc.New(ctx).Latest(j.ID)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	resp.LatestRun = latest

	return c.JSON(http.StatusOK, resp)
}

// embeddedTriggerVisible applies the trigger read rule to the trigger a job
// response embeds. A trigger can serve jobs in several namespaces, and its
// configuration (webhook secrets included) is readable through
// GET /v1/triggers/:id only with viewer in every one of them; a
// namespace-confined caller therefore gets the trigger embedded only when it
// holds viewer in every namespace the trigger serves, and otherwise gets the
// job without it. Unrestricted callers always get it.
func embeddedTriggerVisible(ctx context.Context, triggerID uuid.UUID) (bool, error) {
	if !authz.Restricted(ctx) {
		return true, nil
	}
	namespaces, err := triggerServedNamespaces(ctx, triggerID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	if len(namespaces) == 0 {
		return false, nil
	}
	for _, namespace := range namespaces {
		if !authz.NamespaceAllowed(ctx, namespace) {
			return false, nil
		}
	}
	return true, nil
}

// triggerServedNamespaces is the namespace lookup the trigger routes'
// resolver uses (api/middleware resolveTrigger); a seam for tests.
var triggerServedNamespaces = func(ctx context.Context, triggerID uuid.UUID) ([]string, error) {
	return iauth.NewService(db.Connection()).TriggerJobNamespaces(ctx, triggerID)
}
