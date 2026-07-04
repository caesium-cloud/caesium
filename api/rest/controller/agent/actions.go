package agent

import (
	"errors"
	"net/http"

	agentsvc "github.com/caesium-cloud/caesium/api/rest/service/agent"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// Actions handles POST /v1/agent/incidents/:id/actions — propose/execute a typed
// remediation action. The action is validated and executed by Stream B's
// server-side action executor (the agent never gets shell/SQL/generic HTTP);
// until that executor is wired the action is recorded as `proposed` for the
// audit spine. Tier-3 actions always route to a human approval, never
// auto-execute — that boundary is enforced server-side by the executor, not here.
func Actions(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid incident id")
	}

	var req agentsvc.ActionRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	svc := agentsvc.New(c.Request().Context())
	inc, err := svc.Incident(id)
	if err != nil {
		if errors.Is(err, agentsvc.ErrIncidentNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	result, err := svc.ProposeAction(inc, req)
	if err != nil {
		if errors.Is(err, agentsvc.ErrUnknownActionType) {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	// A proposed (not yet executed) action is accepted for later resolution; an
	// executed/terminal one is a completed mutation.
	status := http.StatusOK
	if result.Disposition == "proposed" || result.Disposition == "awaiting_approval" {
		status = http.StatusAccepted
	}
	return c.JSON(status, result)
}
