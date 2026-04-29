package notification

import (
	"encoding/json"
	"net/http"
	"time"

	svc "github.com/caesium-cloud/caesium/api/rest/service/notification"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

func ListChannels(c *echo.Context) error {
	req, err := parseListRequest(c)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	channels, err := svc.New(c.Request().Context()).ListChannels(req)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	views := make([]channelView, len(channels))
	for i := range channels {
		views[i] = redactChannel(channels[i])
	}
	return c.JSON(http.StatusOK, views)
}

func GetChannel(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	ch, err := svc.New(c.Request().Context()).GetChannel(id)
	if err != nil {
		return serviceError(err)
	}
	return c.JSON(http.StatusOK, redactChannel(*ch))
}

func CreateChannel(c *echo.Context) error {
	req := &svc.CreateChannelRequest{}
	if err := c.Bind(req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	ch, err := svc.New(c.Request().Context()).CreateChannel(req)
	if err != nil {
		return serviceError(err)
	}
	return c.JSON(http.StatusCreated, redactChannel(*ch))
}

func UpdateChannel(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	req := &svc.UpdateChannelRequest{}
	if err := c.Bind(req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	ch, err := svc.New(c.Request().Context()).UpdateChannel(id, req)
	if err != nil {
		return serviceError(err)
	}
	return c.JSON(http.StatusOK, redactChannel(*ch))
}

func DeleteChannel(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	if err := svc.New(c.Request().Context()).DeleteChannel(id); err != nil {
		return serviceError(err)
	}
	return c.NoContent(http.StatusNoContent)
}

// channelView is the API response for a notification channel with
// sensitive config fields redacted.
type channelView struct {
	ID        uuid.UUID              `json:"id"`
	Name      string                 `json:"name"`
	Type      models.ChannelType     `json:"type"`
	Config    map[string]interface{} `json:"config"`
	Enabled   bool                   `json:"enabled"`
	CreatedAt time.Time              `json:"created_at"`
	UpdatedAt time.Time              `json:"updated_at"`
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
	cfg := make(map[string]interface{})
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
