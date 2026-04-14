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

func TestSlackSender_Send(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg, _ := json.Marshal(slackConfig{
		WebhookURL: srv.URL,
		Channel:    "#alerts",
		Username:   "Caesium Bot",
	})

	ch := models.NotificationChannel{
		ID:     uuid.New(),
		Name:   "test-slack",
		Type:   models.ChannelTypeSlack,
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

	sender := NewSlackSender()
	err := sender.Send(context.Background(), ch, payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify Slack message structure.
	var msg slackMessage
	if err := json.Unmarshal(received, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Channel != "#alerts" {
		t.Errorf("channel: got %q, want %q", msg.Channel, "#alerts")
	}
	if msg.Username != "Caesium Bot" {
		t.Errorf("username: got %q, want %q", msg.Username, "Caesium Bot")
	}
	if msg.Text == "" {
		t.Error("text should not be empty")
	}
	if len(msg.Blocks) < 2 {
		t.Fatalf("expected at least 2 blocks, got %d", len(msg.Blocks))
	}
	// First block is header.
	if msg.Blocks[0].Type != "header" {
		t.Errorf("first block type: got %q, want header", msg.Blocks[0].Type)
	}
	// Second block has fields.
	if msg.Blocks[1].Type != "section" {
		t.Errorf("second block type: got %q, want section", msg.Blocks[1].Type)
	}
}

func TestSlackSender_MissingWebhookURL(t *testing.T) {
	cfg, _ := json.Marshal(slackConfig{})
	ch := models.NotificationChannel{
		ID:     uuid.New(),
		Name:   "no-url",
		Type:   models.ChannelTypeSlack,
		Config: cfg,
	}

	sender := NewSlackSender()
	err := sender.Send(context.Background(), ch, Payload{Timestamp: time.Now()})
	if err == nil {
		t.Fatal("expected error for missing webhook_url")
	}
}

func TestSlackSender_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	cfg, _ := json.Marshal(slackConfig{WebhookURL: srv.URL})
	ch := models.NotificationChannel{
		ID:     uuid.New(),
		Name:   "forbidden-slack",
		Type:   models.ChannelTypeSlack,
		Config: cfg,
	}

	sender := NewSlackSender()
	err := sender.Send(context.Background(), ch, Payload{EventType: event.TypeRunFailed, Timestamp: time.Now()})
	if err == nil {
		t.Fatal("expected error for non-200 response")
	}
}

func TestBuildSlackMessage_WithError(t *testing.T) {
	p := Payload{
		EventType: event.TypeTaskFailed,
		JobAlias:  "ingest-pipeline",
		RunID:     uuid.New(),
		Error:     "container OOMKilled",
		Timestamp: time.Now().UTC(),
	}

	msg := buildSlackMessage(slackConfig{}, p)
	if len(msg.Blocks) < 3 {
		t.Fatalf("expected at least 3 blocks (header, fields, error), got %d", len(msg.Blocks))
	}
	// Third block should contain the error.
	errBlock := msg.Blocks[2]
	if errBlock.Text == nil || errBlock.Text.Text == "" {
		t.Error("error block should have text")
	}
}

func TestBuildSlackMessage_NoError(t *testing.T) {
	p := Payload{
		EventType: event.TypeRunCompleted,
		JobAlias:  "report-gen",
		RunID:     uuid.New(),
		Timestamp: time.Now().UTC(),
	}

	msg := buildSlackMessage(slackConfig{}, p)
	// Should have header + fields but no error block.
	if len(msg.Blocks) != 2 {
		t.Errorf("expected 2 blocks (no error), got %d", len(msg.Blocks))
	}
}

func TestFriendlyEventName(t *testing.T) {
	tests := []struct {
		input event.Type
		want  string
	}{
		{event.TypeTaskFailed, "Task Failed"},
		{event.TypeRunFailed, "Run Failed"},
		{event.TypeRunTimedOut, "Run Timed Out"},
		{event.TypeSLAMissed, "SLA Missed"},
		{event.TypeRunCompleted, "Run Completed"},
		{event.TypeTaskSucceeded, "Task Succeeded"},
		{event.Type("unknown"), "unknown"},
	}

	for _, tt := range tests {
		got := friendlyEventName(tt.input)
		if got != tt.want {
			t.Errorf("friendlyEventName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestShortID(t *testing.T) {
	id := uuid.MustParse("12345678-1234-1234-1234-123456789abc")
	got := shortID(id)
	if got != "12345678" {
		t.Errorf("shortID: got %q, want %q", got, "12345678")
	}
}
