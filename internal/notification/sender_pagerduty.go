package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
)

const pagerDutyEventsURL = "https://events.pagerduty.com/v2/enqueue"

// pagerdutyConfig is the expected JSON shape of a PagerDuty channel's Config.
type pagerdutyConfig struct {
	RoutingKey string `json:"routing_key"`
	// Severity overrides the default severity mapping.
	// One of: critical, error, warning, info.
	Severity string `json:"severity,omitempty"`
}

// PagerDutySender sends notifications via PagerDuty Events API v2.
type PagerDutySender struct {
	client *http.Client
}

// NewPagerDutySender creates a PagerDuty notification sender.
func NewPagerDutySender() *PagerDutySender {
	return &PagerDutySender{
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

func (s *PagerDutySender) Send(ctx context.Context, ch models.NotificationChannel, payload Payload) error {
	var cfg pagerdutyConfig
	if err := json.Unmarshal(ch.Config, &cfg); err != nil {
		return fmt.Errorf("pagerduty: invalid channel config: %w", err)
	}

	if cfg.RoutingKey == "" {
		return fmt.Errorf("pagerduty: routing_key is required in channel %q config", ch.Name)
	}

	pdEvent := buildPagerDutyEvent(cfg, payload)

	body, err := json.Marshal(pdEvent)
	if err != nil {
		return fmt.Errorf("pagerduty: marshal event: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, pagerDutyEventsURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("pagerduty: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("pagerduty: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("pagerduty: API responded %d", resp.StatusCode)
}

// pdEvent is the PagerDuty Events API v2 payload.
type pdEvent struct {
	RoutingKey  string    `json:"routing_key"`
	EventAction string    `json:"event_action"`
	DedupKey    string    `json:"dedup_key,omitempty"`
	Payload     pdPayload `json:"payload"`
}

type pdPayload struct {
	Summary   string            `json:"summary"`
	Source    string            `json:"source"`
	Severity  string            `json:"severity"`
	Timestamp string            `json:"timestamp,omitempty"`
	Component string            `json:"component,omitempty"`
	Group     string            `json:"group,omitempty"`
	CustomDetails map[string]interface{} `json:"custom_details,omitempty"`
}

func buildPagerDutyEvent(cfg pagerdutyConfig, p Payload) pdEvent {
	severity := cfg.Severity
	if severity == "" {
		severity = defaultPDSeverity(p.EventType)
	}

	summary := fmt.Sprintf("[Caesium] %s", friendlyEventName(p.EventType))
	if p.JobAlias != "" {
		summary = fmt.Sprintf("[Caesium] %s: %s", friendlyEventName(p.EventType), p.JobAlias)
	}
	if p.Error != "" {
		errSnippet := p.Error
		if len(errSnippet) > 200 {
			errSnippet = errSnippet[:200] + "…"
		}
		summary = fmt.Sprintf("%s — %s", summary, errSnippet)
	}
	// PagerDuty summary is capped at 1024 chars.
	if len(summary) > 1024 {
		summary = summary[:1021] + "..."
	}

	// Dedup key prevents duplicate pages for the same run failure.
	dedupKey := fmt.Sprintf("caesium-%s-%s-%s", p.EventType, p.JobID, p.RunID)

	details := map[string]interface{}{
		"event_type": string(p.EventType),
		"job_id":     p.JobID.String(),
		"run_id":     p.RunID.String(),
	}
	if p.TaskID.String() != "00000000-0000-0000-0000-000000000000" {
		details["task_id"] = p.TaskID.String()
	}
	if p.Error != "" {
		details["error"] = p.Error
	}

	action := "trigger"
	if p.EventType == event.TypeRunCompleted || p.EventType == event.TypeTaskSucceeded {
		action = "resolve"
	}

	return pdEvent{
		RoutingKey:  cfg.RoutingKey,
		EventAction: action,
		DedupKey:    dedupKey,
		Payload: pdPayload{
			Summary:       summary,
			Source:        "caesium",
			Severity:      severity,
			Timestamp:     p.Timestamp.Format(time.RFC3339),
			Component:     p.JobAlias,
			CustomDetails: details,
		},
	}
}

func defaultPDSeverity(t event.Type) string {
	switch t {
	case event.TypeTaskFailed, event.TypeRunFailed:
		return "error"
	case event.TypeRunTimedOut:
		return "error"
	case event.TypeSLAMissed:
		return "warning"
	case event.TypeRunCompleted, event.TypeTaskSucceeded:
		return "info"
	default:
		return "warning"
	}
}
