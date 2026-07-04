// Package incident implements the operator-facing incident REST surface
// (agent-in-the-loop D1/D2): the read API (GET /v1/incidents, GET /:id) and the
// tier-3 approval decisions (POST /:id/approvals/:approval_id/{approve,reject}).
package incident

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	svc "github.com/caesium-cloud/caesium/api/rest/service/incident"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// Controller serves the incident endpoints. It holds the event bus so approval
// decisions can emit incident lifecycle events on the existing /events stream.
type Controller struct {
	bus event.Bus
}

// New constructs an incident Controller bound to the event bus.
func New(bus event.Bus) *Controller {
	return &Controller{bus: bus}
}

// List handles GET /v1/incidents — the bounded, paginated, filterable feed.
func (ctrl *Controller) List(c *echo.Context) error {
	params := svc.ListParams{
		Status:        strings.TrimSpace(c.QueryParam("status")),
		Class:         strings.TrimSpace(c.QueryParam("class")),
		NeedsApproval: parseBool(c.QueryParam("needs_approval")),
	}

	if raw := strings.TrimSpace(c.QueryParam("job_id")); raw != "" {
		jobID, err := uuid.Parse(raw)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid job_id")
		}
		params.JobID = &jobID
	}
	if raw := strings.TrimSpace(c.QueryParam("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid limit")
		}
		params.Limit = n
	}
	if raw := strings.TrimSpace(c.QueryParam("offset")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid offset")
		}
		params.Offset = n
	}

	result, err := svc.New(c.Request().Context()).List(params)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, result)
}

// Get handles GET /v1/incidents/:id — the full triage timeline.
func (ctrl *Controller) Get(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	detail, err := svc.New(c.Request().Context()).Get(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, detail)
}

// parseBool treats "1", "true", "yes", "on" (case-insensitive) as true.
func parseBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
