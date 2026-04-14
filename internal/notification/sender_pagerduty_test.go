package notification

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

func TestPagerDutySender_BuildEvent(t *testing.T) {
	cfg := pagerdutyConfig{
		RoutingKey: "test-routing-key",
		Severity:   "critical",
	}

	payload := Payload{
		EventType: event.TypeRunFailed,
		JobID:     uuid.New(),
		RunID:     uuid.New(),
		JobAlias:  "etl-daily",
		Error:     "exit code 1",
		Timestamp: time.Now().UTC(),
	}

	evt := buildPagerDutyEvent(cfg, payload)

	if evt.RoutingKey != "test-routing-key" {
		t.Errorf("routing key: got %q, want %q", evt.RoutingKey, "test-routing-key")
	}
	if evt.EventAction != "trigger" {
		t.Errorf("action: got %q, want %q", evt.EventAction, "trigger")
	}
	if evt.Payload.Severity != "critical" {
		t.Errorf("severity: got %q, want %q", evt.Payload.Severity, "critical")
	}
	if evt.Payload.Source != "caesium" {
		t.Errorf("source: got %q, want %q", evt.Payload.Source, "caesium")
	}
	if evt.Payload.Component != "etl-daily" {
		t.Errorf("component: got %q, want %q", evt.Payload.Component, "etl-daily")
	}
	if evt.DedupKey == "" {
		t.Error("dedup_key should not be empty")
	}
}

func TestBuildPagerDutyEvent_ResolveAction(t *testing.T) {
	cfg := pagerdutyConfig{RoutingKey: "key"}
	p := Payload{
		EventType: event.TypeRunCompleted,
		JobID:     uuid.New(),
		RunID:     uuid.New(),
		Timestamp: time.Now(),
	}
	evt := buildPagerDutyEvent(cfg, p)
	if evt.EventAction != "resolve" {
		t.Errorf("action for run_completed: got %q, want resolve", evt.EventAction)
	}
}

func TestBuildPagerDutyEvent_DefaultSeverity(t *testing.T) {
	tests := []struct {
		eventType event.Type
		want      string
	}{
		{event.TypeTaskFailed, "error"},
		{event.TypeRunFailed, "error"},
		{event.TypeRunTimedOut, "error"},
		{event.TypeSLAMissed, "warning"},
		{event.TypeRunCompleted, "info"},
		{event.TypeTaskSucceeded, "info"},
	}

	for _, tt := range tests {
		t.Run(string(tt.eventType), func(t *testing.T) {
			got := defaultPDSeverity(tt.eventType)
			if got != tt.want {
				t.Errorf("defaultPDSeverity(%q) = %q, want %q", tt.eventType, got, tt.want)
			}
		})
	}
}

func TestBuildPagerDutyEvent_SummaryTruncation(t *testing.T) {
	longError := make([]byte, 2000)
	for i := range longError {
		longError[i] = 'x'
	}

	cfg := pagerdutyConfig{RoutingKey: "key"}
	p := Payload{
		EventType: event.TypeRunFailed,
		JobAlias:  "my-job",
		Error:     string(longError),
		Timestamp: time.Now(),
	}
	evt := buildPagerDutyEvent(cfg, p)
	if len(evt.Payload.Summary) > 1024 {
		t.Errorf("summary length %d exceeds 1024", len(evt.Payload.Summary))
	}
}

func TestPagerDutySender_MissingRoutingKey(t *testing.T) {
	cfg, _ := json.Marshal(pagerdutyConfig{})
	ch := models.NotificationChannel{
		ID:     uuid.New(),
		Name:   "no-key",
		Type:   models.ChannelTypePagerDuty,
		Config: cfg,
	}

	sender := NewPagerDutySender()
	err := sender.Send(context.Background(), ch, Payload{Timestamp: time.Now()})
	if err == nil {
		t.Fatal("expected error for missing routing_key")
	}
}
