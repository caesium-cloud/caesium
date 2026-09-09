package notification

import (
	"encoding/json"
	"net/http"
	"time"

	svc "github.com/caesium-cloud/caesium/api/rest/service/notification"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// newService constructs the notification service used by the handlers below.
// It is a package-level var (rather than a direct svc.New call) so tests can
// substitute a fake Service without a live database connection — the same
// seam api/rest/controller/webhook uses for its collaborators.
var newService = svc.New

func (ctrl *Controller) ListChannels(c *echo.Context) error {
	req, err := parseListRequest(c)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	channels, err := newService(c.Request().Context()).ListChannels(req)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	views := make([]channelView, len(channels))
	for i := range channels {
		views[i] = redactChannel(channels[i])
	}
	return c.JSON(http.StatusOK, views)
}

func (ctrl *Controller) GetChannel(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	ch, err := newService(c.Request().Context()).GetChannel(id)
	if err != nil {
		return serviceError(err)
	}
	return c.JSON(http.StatusOK, redactChannel(*ch))
}

func (ctrl *Controller) CreateChannel(c *echo.Context) error {
	req := &svc.CreateChannelRequest{}
	if err := c.Bind(req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	ch, err := newService(c.Request().Context()).CreateChannel(req)
	if err != nil {
		return serviceError(err)
	}

	logAuditFailure(ctrl.auditor.Log(auth.AuditEntry{
		Actor:        auditActor(c),
		Action:       auth.ActionNotificationChannelCreate,
		ResourceType: "notification_channel",
		ResourceID:   ch.ID.String(),
		SourceIP:     c.RealIP(),
		Outcome:      auth.OutcomeSuccess,
		Metadata: map[string]any{
			"name": ch.Name,
			"type": string(ch.Type),
		},
	}))

	return c.JSON(http.StatusCreated, redactChannel(*ch))
}

func (ctrl *Controller) UpdateChannel(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	req := &svc.UpdateChannelRequest{}
	if err := c.Bind(req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	ch, err := newService(c.Request().Context()).UpdateChannel(id, req)
	if err != nil {
		return serviceError(err)
	}

	// Metadata deliberately excludes req.Config / ch.Config: channel config
	// carries secrets (webhook URLs, tokens) that the read API itself
	// redacts (see redactChannel) — the audit trail must not become the
	// leak path for values the REST responses go out of their way to mask.
	logAuditFailure(ctrl.auditor.Log(auth.AuditEntry{
		Actor:        auditActor(c),
		Action:       auth.ActionNotificationChannelUpdate,
		ResourceType: "notification_channel",
		ResourceID:   ch.ID.String(),
		SourceIP:     c.RealIP(),
		Outcome:      auth.OutcomeSuccess,
		Metadata: map[string]any{
			"name":           ch.Name,
			"enabled":        ch.Enabled,
			"config_updated": req.Config != nil,
		},
	}))

	return c.JSON(http.StatusOK, redactChannel(*ch))
}

func (ctrl *Controller) DeleteChannel(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	if err := newService(c.Request().Context()).DeleteChannel(id); err != nil {
		return serviceError(err)
	}

	logAuditFailure(ctrl.auditor.Log(auth.AuditEntry{
		Actor:        auditActor(c),
		Action:       auth.ActionNotificationChannelDelete,
		ResourceType: "notification_channel",
		ResourceID:   id.String(),
		SourceIP:     c.RealIP(),
		Outcome:      auth.OutcomeSuccess,
	}))

	return c.NoContent(http.StatusNoContent)
}

// channelView is the API response for a notification channel with
// sensitive config fields redacted.
type channelView struct {
	ID        uuid.UUID          `json:"id"`
	Name      string             `json:"name"`
	Type      models.ChannelType `json:"type"`
	Config    map[string]any     `json:"config"`
	Enabled   bool               `json:"enabled"`
	CreatedAt time.Time          `json:"created_at"`
	UpdatedAt time.Time          `json:"updated_at"`
}

// sensitiveKeys are config keys whose values must be redacted in API responses.
var sensitiveKeys = map[string]struct{}{
	"password":    {},
	"routing_key": {},
	"webhook_url": {},
	"url":         {},
	"token":       {},
	"secret":      {},
	"api_key":     {},
}

func redactChannel(ch models.NotificationChannel) channelView {
	cfg := make(map[string]any)
	if len(ch.Config) > 0 {
		_ = json.Unmarshal(ch.Config, &cfg)
	}

	for key, val := range cfg {
		if _, sensitive := sensitiveKeys[key]; sensitive {
			if s, ok := val.(string); ok && len(s) > 0 {
				cfg[key] = maskString(s)
			}
		}
	}

	return channelView{
		ID:        ch.ID,
		Name:      ch.Name,
		Type:      ch.Type,
		Config:    cfg,
		Enabled:   ch.Enabled,
		CreatedAt: ch.CreatedAt,
		UpdatedAt: ch.UpdatedAt,
	}
}

// maskString replaces the middle of a string with asterisks, keeping the
// first 4 and last 4 characters visible. For short strings (<= 8 chars)
// the entire value is masked.
func maskString(s string) string {
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "****" + s[len(s)-4:]
}
