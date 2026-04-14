package notification

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

func TestWebhookSender_Send(t *testing.T) {
	var received []byte
	var receivedHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg, _ := json.Marshal(webhookConfig{
		URL:     srv.URL,
		Headers: map[string]string{"X-Custom": "test-value"},
	})

	ch := models.NotificationChannel{
		ID:     uuid.New(),
		Name:   "test-webhook",
		Type:   models.ChannelTypeWebhook,
		Config: cfg,
	}

	payload := Payload{
		EventType: event.TypeRunFailed,
		JobID:     uuid.New(),
		RunID:     uuid.New(),
		JobAlias:  "etl-daily",
		Error:     "exit code 1",
		Timestamp: time.Now().UTC(),
	}

	sender := NewWebhookSender()
	err := sender.Send(context.Background(), ch, payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify payload was received.
	var got Payload
	if err := json.Unmarshal(received, &got); err != nil {
		t.Fatalf("unmarshal received body: %v", err)
	}
	if got.EventType != event.TypeRunFailed {
		t.Errorf("event type: got %q, want %q", got.EventType, event.TypeRunFailed)
	}
	if got.JobAlias != "etl-daily" {
		t.Errorf("job alias: got %q, want %q", got.JobAlias, "etl-daily")
	}

	// Verify custom header.
	if receivedHeaders.Get("X-Custom") != "test-value" {
		t.Errorf("X-Custom header: got %q, want %q", receivedHeaders.Get("X-Custom"), "test-value")
	}
	if receivedHeaders.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type: got %q", receivedHeaders.Get("Content-Type"))
	}
}

func TestWebhookSender_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg, _ := json.Marshal(webhookConfig{URL: srv.URL})
	ch := models.NotificationChannel{
		ID:     uuid.New(),
		Name:   "bad-webhook",
		Type:   models.ChannelTypeWebhook,
		Config: cfg,
	}

	sender := NewWebhookSender()
	err := sender.Send(context.Background(), ch, Payload{EventType: event.TypeRunFailed, Timestamp: time.Now()})
	if err == nil {
		t.Fatal("expected error for non-2xx response")
	}
}

func TestWebhookSender_MissingURL(t *testing.T) {
	cfg, _ := json.Marshal(webhookConfig{})
	ch := models.NotificationChannel{
		ID:     uuid.New(),
		Name:   "no-url",
		Type:   models.ChannelTypeWebhook,
		Config: cfg,
	}

	sender := NewWebhookSender()
	err := sender.Send(context.Background(), ch, Payload{Timestamp: time.Now()})
	if err == nil {
		t.Fatal("expected error for missing URL")
	}
}

func TestWebhookSender_CustomMethod(t *testing.T) {
	var receivedMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg, _ := json.Marshal(webhookConfig{URL: srv.URL, Method: "PUT"})
	ch := models.NotificationChannel{
		ID:     uuid.New(),
		Name:   "put-webhook",
		Type:   models.ChannelTypeWebhook,
		Config: cfg,
	}

	sender := NewWebhookSender()
	if err := sender.Send(context.Background(), ch, Payload{Timestamp: time.Now()}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedMethod != "PUT" {
		t.Errorf("method: got %q, want PUT", receivedMethod)
	}
}

func TestWebhookSender_InvalidConfig(t *testing.T) {
	ch := models.NotificationChannel{
		ID:     uuid.New(),
		Name:   "bad-config",
		Type:   models.ChannelTypeWebhook,
		Config: []byte("not json"),
	}
	sender := NewWebhookSender()
	err := sender.Send(context.Background(), ch, Payload{Timestamp: time.Now()})
	if err == nil {
		t.Fatal("expected error for invalid config JSON")
	}
}
