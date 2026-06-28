package event

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	triggerevent "github.com/caesium-cloud/caesium/internal/trigger/event"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

func TestIngestRoutesEventWithAPIKey(t *testing.T) {
	t.Setenv("CAESIUM_EVENT_INGEST_API_KEY", "test-key")
	require.NoError(t, env.Process())

	original := routeEvent
	routeEvent = func(_ context.Context, evt *models.IngestedEvent) (*triggerevent.RouteResult, error) {
		require.Equal(t, "order.created", evt.Type)
		require.Equal(t, "integration", evt.Source)
		require.JSONEq(t, `{"id":"ord-1"}`, string(evt.Data))
		return &triggerevent.RouteResult{
			EventID:   uuid.New(),
			EventType: evt.Type,
			Source:    evt.Source,
			MatchedTriggers: []triggerevent.TriggerRouteResult{
				{TriggerID: uuid.New(), RunsStarted: []uuid.UUID{uuid.New(), uuid.New()}},
			},
		}, nil
	}
	t.Cleanup(func() { routeEvent = original })

	req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(`{
		"type":"order.created",
		"source":"integration",
		"data":{"id":"ord-1"}
	}`))
	req.Header.Set("X-Caesium-API-Key", "test-key")
	rec := httptest.NewRecorder()

	err := New(nil).Ingest(echo.New().NewContext(req, rec))
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, rec.Code)

	var resp ingestResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEqual(t, uuid.Nil, resp.EventID)
	require.Equal(t, 1, resp.MatchedTriggers)
	require.Equal(t, 2, resp.RunsStarted)
}

func TestIngestRejectsMissingAPIKey(t *testing.T) {
	t.Setenv("CAESIUM_EVENT_INGEST_API_KEY", "test-key")
	require.NoError(t, env.Process())

	req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(`{"type":"order.created","data":{}}`))
	rec := httptest.NewRecorder()

	err := New(nil).Ingest(echo.New().NewContext(req, rec))
	require.Error(t, err)
	httpErr, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusUnauthorized, httpErr.Code)
}
