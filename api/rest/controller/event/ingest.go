package event

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	eventsvc "github.com/caesium-cloud/caesium/api/rest/service/event"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	triggerevent "github.com/caesium-cloud/caesium/internal/trigger/event"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/datatypes"
)

type ingestRequest struct {
	Type   string          `json:"type"`
	Source string          `json:"source"`
	Data   json.RawMessage `json:"data"`
}

type ingestResponse struct {
	// EventID is unique per accepted request. Event ingestion is at-least-once:
	// retrying the same payload can create another event and route another run.
	EventID         uuid.UUID `json:"event_id"`
	MatchedTriggers int       `json:"matched_triggers"`
	RunsStarted     int       `json:"runs_started"`
}

var routeEvent = func(ctx context.Context, evt *models.IngestedEvent) (*triggerevent.RouteResult, error) {
	return triggerevent.DefaultRouter().Route(ctx, evt)
}

var eventIngestRateLimiters = authmw.NewIPRateLimiters(15*time.Minute, eventIngestRateLimitConfig)

// Ingest accepts client-supplied events using at-least-once semantics. A
// successful response means this request was persisted and routed; repeating an
// identical request may create another event and start another run.
func (ctrl *Controller) Ingest(c *echo.Context) error {
	if !eventIngestRateLimiters.Allow(c.RealIP()) {
		return echo.NewHTTPError(http.StatusTooManyRequests, "rate limit exceeded")
	}
	if err := requireEventIngestAPIKey(c); err != nil {
		return err
	}

	body, err := readIngestBody(c.Request().Body)
	switch {
	case errors.Is(err, errIngestRequestTooLarge):
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "request body too large")
	case err != nil:
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	req := &ingestRequest{}
	if err := json.Unmarshal(body, req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	data := req.Data
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	if !json.Valid(data) {
		return echo.NewHTTPError(http.StatusBadRequest, "data must be valid json")
	}

	evt := &models.IngestedEvent{
		Type:   strings.TrimSpace(req.Type),
		Source: strings.TrimSpace(req.Source),
		Data:   datatypes.JSON(data),
	}
	if evt.Type == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "type is required")
	}

	result, err := routeEvent(c.Request().Context(), evt)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	if result == nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error")
	}
	metrics.EventsIngestedTotal.WithLabelValues("ingest").Inc()

	return c.JSON(http.StatusAccepted, ingestResponse{
		EventID:         result.EventID,
		MatchedTriggers: len(result.MatchedTriggers),
		RunsStarted:     runsStarted(result),
	})
}

func (ctrl *Controller) ListIngested(c *echo.Context) error {
	req, err := eventsvc.ListRequestFromValues(c.QueryParams())
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	events, err := eventsvc.New(c.Request().Context()).ListIngested(req)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, events)
}

func eventIngestRateLimitConfig() (int, int) {
	vars := env.Variables()
	return vars.WebhookRateLimitPerMinute, vars.WebhookRateLimitBurst
}

var errIngestRequestTooLarge = errors.New("ingest request body too large")

func readIngestBody(body io.Reader) ([]byte, error) {
	maxBytes := env.Variables().WebhookMaxBodySize.Int64()
	if maxBytes <= 0 {
		return io.ReadAll(body)
	}

	limited := io.LimitReader(body, maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errIngestRequestTooLarge
	}
	return data, nil
}

func requireEventIngestAPIKey(c *echo.Context) error {
	expected := strings.TrimSpace(env.Variables().EventIngestAPIKey)
	if expected == "" {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "event ingest is disabled")
	}

	provided := strings.TrimSpace(c.Request().Header.Get("X-Caesium-API-Key"))
	if provided == "" {
		auth := strings.TrimSpace(c.Request().Header.Get("Authorization"))
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			provided = strings.TrimSpace(auth[7:])
		}
	}

	if subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
		return echo.NewHTTPError(http.StatusUnauthorized, "invalid api key")
	}
	return nil
}

func runsStarted(result *triggerevent.RouteResult) int {
	if result == nil {
		return 0
	}
	var total int
	for _, match := range result.MatchedTriggers {
		total += len(match.RunsStarted)
	}
	return total
}
