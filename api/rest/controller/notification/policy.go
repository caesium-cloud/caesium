package notification

import (
	"encoding/json"
	"net/http"

	svc "github.com/caesium-cloud/caesium/api/rest/service/notification"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

func (ctrl *Controller) ListPolicies(c *echo.Context) error {
	req, err := parseListRequest(c)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	policies, err := newService(c.Request().Context()).ListPolicies(req)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, policies)
}

func (ctrl *Controller) GetPolicy(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	p, err := newService(c.Request().Context()).GetPolicy(id)
	if err != nil {
		return serviceError(err)
	}
	return c.JSON(http.StatusOK, p)
}

func (ctrl *Controller) CreatePolicy(c *echo.Context) error {
	req := &svc.CreatePolicyRequest{}
	if err := c.Bind(req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	p, err := newService(c.Request().Context()).CreatePolicy(req)
	if err != nil {
		return serviceError(err)
	}

	logAuditFailure(ctrl.auditor.Log(auth.AuditEntry{
		Actor:        auditActor(c),
		Action:       auth.ActionNotificationPolicyCreate,
		ResourceType: "notification_policy",
		ResourceID:   p.ID.String(),
		SourceIP:     c.RealIP(),
		Outcome:      auth.OutcomeSuccess,
		Metadata: map[string]any{
			"name":        p.Name,
			"channel_id":  p.ChannelID.String(),
			"event_types": req.EventTypes,
		},
	}))

	return c.JSON(http.StatusCreated, p)
}

func (ctrl *Controller) UpdatePolicy(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	req := &svc.UpdatePolicyRequest{}
	if err := c.Bind(req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	p, err := newService(c.Request().Context()).UpdatePolicy(id, req)
	if err != nil {
		return serviceError(err)
	}

	logAuditFailure(ctrl.auditor.Log(auth.AuditEntry{
		Actor:        auditActor(c),
		Action:       auth.ActionNotificationPolicyUpdate,
		ResourceType: "notification_policy",
		ResourceID:   p.ID.String(),
		SourceIP:     c.RealIP(),
		Outcome:      auth.OutcomeSuccess,
		Metadata: map[string]any{
			"name":        p.Name,
			"channel_id":  p.ChannelID.String(),
			"event_types": decodeEventTypes(p),
			"enabled":     p.Enabled,
		},
	}))

	return c.JSON(http.StatusOK, p)
}

// decodeEventTypes unmarshals a policy's persisted event types for audit
// metadata, so the log carries a clean JSON array rather than the raw
// datatypes.JSON bytes. It never fails the request: a decode error just
// omits the field.
func decodeEventTypes(p *models.NotificationPolicy) []string {
	if p == nil || len(p.EventTypes) == 0 {
		return nil
	}
	var types []string
	if err := json.Unmarshal(p.EventTypes, &types); err != nil {
		return nil
	}
	return types
}

func (ctrl *Controller) DeletePolicy(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	if err := newService(c.Request().Context()).DeletePolicy(id); err != nil {
		return serviceError(err)
	}

	logAuditFailure(ctrl.auditor.Log(auth.AuditEntry{
		Actor:        auditActor(c),
		Action:       auth.ActionNotificationPolicyDelete,
		ResourceType: "notification_policy",
		ResourceID:   id.String(),
		SourceIP:     c.RealIP(),
		Outcome:      auth.OutcomeSuccess,
	}))

	return c.NoContent(http.StatusNoContent)
}
