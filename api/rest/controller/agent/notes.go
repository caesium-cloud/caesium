package agent

import (
	"errors"
	"net/http"
	"strings"

	agentsvc "github.com/caesium-cloud/caesium/api/rest/service/agent"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// noteBody is the POST /v1/agent/incidents/:id/notes request body.
type noteBody struct {
	Text string `json:"text"`
}

// Notes handles POST /v1/agent/incidents/:id/notes — append a free-text finding
// to the incident timeline (recorded as a tier-0 AgentAction of type "note").
func Notes(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid incident id")
	}

	var body noteBody
	if err := c.Bind(&body); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}
	body.Text = strings.TrimSpace(body.Text)
	if body.Text == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "text is required")
	}

	svc := agentsvc.New(c.Request().Context())
	inc, err := svc.Incident(id)
	if err != nil {
		if errors.Is(err, agentsvc.ErrIncidentNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	action, err := svc.Note(inc, body.Text)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusCreated, action)
}
