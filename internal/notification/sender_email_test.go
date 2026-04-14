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

func TestEmailSender_MissingSMTPHost(t *testing.T) {
	cfg, _ := json.Marshal(emailConfig{
		From: "caesium@example.com",
		To:   []string{"alert@example.com"},
	})
	ch := models.NotificationChannel{
		ID:     uuid.New(),
		Name:   "no-host",
		Type:   models.ChannelTypeEmail,
		Config: cfg,
	}

	sender := NewEmailSender()
	err := sender.Send(context.Background(), ch, Payload{Timestamp: time.Now()})
	if err == nil {
		t.Fatal("expected error for missing smtp_host")
	}
}

func TestEmailSender_MissingFrom(t *testing.T) {
	cfg, _ := json.Marshal(emailConfig{
		SMTPHost: "smtp.example.com",
		To:       []string{"alert@example.com"},
	})
	ch := models.NotificationChannel{
		ID:     uuid.New(),
		Name:   "no-from",
		Type:   models.ChannelTypeEmail,
		Config: cfg,
	}

	sender := NewEmailSender()
	err := sender.Send(context.Background(), ch, Payload{Timestamp: time.Now()})
	if err == nil {
		t.Fatal("expected error for missing from")
	}
}

func TestEmailSender_MissingTo(t *testing.T) {
	cfg, _ := json.Marshal(emailConfig{
		SMTPHost: "smtp.example.com",
		From:     "caesium@example.com",
	})
	ch := models.NotificationChannel{
		ID:     uuid.New(),
		Name:   "no-to",
		Type:   models.ChannelTypeEmail,
		Config: cfg,
	}

	sender := NewEmailSender()
	err := sender.Send(context.Background(), ch, Payload{Timestamp: time.Now()})
	if err == nil {
		t.Fatal("expected error for missing to")
	}
}

func TestEmailSender_InvalidConfig(t *testing.T) {
	ch := models.NotificationChannel{
		ID:     uuid.New(),
		Name:   "bad",
		Type:   models.ChannelTypeEmail,
		Config: []byte("not json"),
	}

	sender := NewEmailSender()
	err := sender.Send(context.Background(), ch, Payload{Timestamp: time.Now()})
	if err == nil {
		t.Fatal("expected error for invalid config")
	}
}

func TestFormatEmailSubject(t *testing.T) {
	tests := []struct {
		name     string
		payload  Payload
		contains string
	}{
		{
			name:     "with alias",
			payload:  Payload{EventType: event.TypeRunFailed, JobAlias: "etl-daily"},
			contains: "Run Failed: etl-daily",
		},
		{
			name:     "without alias",
			payload:  Payload{EventType: event.TypeSLAMissed},
			contains: "SLA Missed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatEmailSubject(tt.payload)
			if !containsStr(got, tt.contains) {
				t.Errorf("subject %q does not contain %q", got, tt.contains)
			}
			if !containsStr(got, "[Caesium]") {
				t.Errorf("subject %q does not contain [Caesium] prefix", got)
			}
		})
	}
}

func TestFormatEmailBody(t *testing.T) {
	jobID := uuid.New()
	runID := uuid.New()
	p := Payload{
		EventType: event.TypeTaskFailed,
		JobID:     jobID,
		RunID:     runID,
		JobAlias:  "pipeline-x",
		Error:     "segfault",
		Timestamp: time.Now().UTC(),
	}

	body := formatEmailBody(p)
	if !containsStr(body, "Task Failed") {
		t.Error("body should contain event name")
	}
	if !containsStr(body, "pipeline-x") {
		t.Error("body should contain job alias")
	}
	if !containsStr(body, jobID.String()) {
		t.Error("body should contain job ID")
	}
	if !containsStr(body, runID.String()) {
		t.Error("body should contain run ID")
	}
	if !containsStr(body, "segfault") {
		t.Error("body should contain error")
	}
}

func TestBuildMIMEMessage(t *testing.T) {
	msg := buildMIMEMessage("from@example.com", []string{"to@example.com"}, "Test Subject", "Body text")
	s := string(msg)
	if !containsStr(s, "From: from@example.com") {
		t.Error("missing From header")
	}
	if !containsStr(s, "To: to@example.com") {
		t.Error("missing To header")
	}
	if !containsStr(s, "Subject: Test Subject") {
		t.Error("missing Subject header")
	}
	if !containsStr(s, "Content-Type: text/plain") {
		t.Error("missing Content-Type header")
	}
	if !containsStr(s, "Body text") {
		t.Error("missing body")
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || findSubstr(s, substr))
}

func findSubstr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
