package trigger

import (
	"net/http"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	triggersvc "github.com/caesium-cloud/caesium/api/rest/service/trigger"
	internaljobdef "github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

func Patch(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	req := &triggersvc.UpdateRequest{}
	if err := c.Bind(req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}
	// Trigger tenancy (B9): the writer's scope, re-checked by the service
	// inside its transaction against every job the trigger serves.
	namespaces, aliases, err := authmw.WriteConfinement(authmw.GetPrincipal(c), models.RoleOperator)
	if err != nil {
		return echo.NewHTTPError(http.StatusForbidden, "insufficient permissions").Wrap(err)
	}
	req.Scope = internaljobdef.NewTenancyScope(namespaces, aliases)

	trigger, err := triggerServiceFactory(c.Request().Context()).Update(id, req)
	switch {
	case err != nil:
		if denial, conflict := internaljobdef.WriteRefusal(err, req.Scope); denial != nil {
			return denial
		} else if conflict != "" {
			return echo.NewHTTPError(http.StatusConflict, conflict).Wrap(err)
		}
		return triggerServiceError(err)
	default:
		if err := triggersvc.NotifyMutation(c.Request().Context()); err != nil {
			log.Warn("event trigger router reload failed after trigger update", "trigger_id", id, "error", err)
		}
		return c.JSON(http.StatusOK, trigger)
	}
}
