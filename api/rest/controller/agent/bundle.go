// Package agent implements the scoped /v1/agent/* tool surface controllers: the
// triage bundle, read-only context passthroughs, typed-action proposals, and
// timeline notes. Every route is gated by the auth middleware's agent-scope
// switch (api/middleware/auth_scope.go), which 403s an agent-session token on
// any incident but the one its credential was minted for.
package agent

import (
	"errors"
	"net/http"

	agentsvc "github.com/caesium-cloud/caesium/api/rest/service/agent"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// Bundle handles GET /v1/agent/incidents/:id/bundle — the JSON triage document
// the agent fetches once at startup.
func Bundle(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid incident id")
	}

	b, err := agentsvc.New(c.Request().Context()).Bundle(id)
	if err != nil {
		if errors.Is(err, agentsvc.ErrIncidentNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, b)
}
