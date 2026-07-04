package agent

import (
	"errors"
	"net/http"
	"strings"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	agentsvc "github.com/caesium-cloud/caesium/api/rest/service/agent"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// Context handles GET /v1/agent/incidents/:id/context/* — the read-only
// passthroughs (logs, why, run history) scoped to the incident's frozen job
// allowlist. The wildcard subpath selects the passthrough. Cross-job reads are
// gated against the allowlist injected by the auth middleware.
func Context(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid incident id")
	}

	svc := agentsvc.New(c.Request().Context())
	inc, err := svc.Incident(id)
	if err != nil {
		if errors.Is(err, agentsvc.ErrIncidentNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	allowed := authmw.GetAllowedJobAliases(c)
	sub := strings.Trim(c.Param("*"), "/")

	switch sub {
	case "logs":
		text, ok, err := svc.FailingLog(inc)
		if err != nil {
			return mapContextError(err)
		}
		return c.JSON(http.StatusOK, map[string]any{"log_tail": text, "available": ok, "scrubbed": true})

	case "history":
		runs, err := svc.History(inc, strings.TrimSpace(c.QueryParam("job")), allowed)
		if err != nil {
			return mapContextError(err)
		}
		return c.JSON(http.StatusOK, map[string]any{"runs": runs})

	case "why":
		task := strings.TrimSpace(c.QueryParam("task"))
		if task == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "task query parameter is required")
		}
		explanation, err := svc.Why(inc, task)
		if err != nil {
			return mapContextError(err)
		}
		return c.JSON(http.StatusOK, explanation)

	default:
		return echo.NewHTTPError(http.StatusNotFound, "unknown context passthrough")
	}
}

func mapContextError(err error) error {
	switch {
	case errors.Is(err, agentsvc.ErrForbiddenJob):
		return echo.NewHTTPError(http.StatusForbidden, agentsvc.ErrForbiddenJob.Error())
	case errors.Is(err, agentsvc.ErrNoFailingRun), errors.Is(err, gorm.ErrRecordNotFound), errors.Is(err, runstorage.ErrTaskRunNotFound):
		return echo.ErrNotFound
	default:
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
}
