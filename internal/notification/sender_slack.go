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
	"github.com/google/uuid"
)

// slackConfig is the expected JSON shape of a Slack channel's Config.
type slackConfig struct {
	WebhookURL string `json:"webhook_url"`
	Channel    string `json:"channel,omitempty"`  // optional override
	Username   string `json:"username,omitempty"` // optional bot name
	IconEmoji  string `json:"icon_emoji,omitempty"`
	Timeout    string `json:"timeout,omitempty"` // Go duration string
}

// SlackSender sends notifications via Slack incoming webhooks.
type SlackSender struct {
	client *http.Client
}

// NewSlackSender creates a Slack notification sender.
func NewSlackSender() *SlackSender {
	return &SlackSender{
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

func (s *SlackSender) Send(ctx context.Context, ch models.NotificationChannel, payload Payload) error {
	var cfg slackConfig
	if err := json.Unmarshal(ch.Config, &cfg); err != nil {
		return fmt.Errorf("slack: invalid channel config: %w", err)
	}

	if cfg.WebhookURL == "" {
		return fmt.Errorf("slack: webhook_url is required in channel %q config", ch.Name)
	}

	timeout := 10 * time.Second
	if cfg.Timeout != "" {
		if d, err := time.ParseDuration(cfg.Timeout); err == nil && d > 0 {
			timeout = d
		}
	}

	msg := buildSlackMessage(cfg, payload)

	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("slack: marshal message: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("slack: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("slack: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode == http.StatusOK {
		return nil
	}
	return fmt.Errorf("slack: webhook responded %d", resp.StatusCode)
}

// slackMessage is the Slack incoming webhook payload.
type slackMessage struct {
	Channel   string       `json:"channel,omitempty"`
	Username  string       `json:"username,omitempty"`
	IconEmoji string       `json:"icon_emoji,omitempty"`
	Text      string       `json:"text"`
	Blocks    []slackBlock `json:"blocks,omitempty"`
}

type slackBlock struct {
	Type string      `json:"type"`
	Text *slackText  `json:"text,omitempty"`
	Fields []slackText `json:"fields,omitempty"`
}

type slackText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func buildSlackMessage(cfg slackConfig, p Payload) slackMessage {
	emoji := eventEmoji(p.EventType)
	title := fmt.Sprintf("%s *%s*", emoji, friendlyEventName(p.EventType))

	msg := slackMessage{
		Channel:   cfg.Channel,
		Username:  cfg.Username,
		IconEmoji: cfg.IconEmoji,
		Text:      fmt.Sprintf("%s — %s", title, p.JobAlias),
	}

	// Header block.
	blocks := []slackBlock{
		{
			Type: "header",
			Text: &slackText{Type: "plain_text", Text: fmt.Sprintf("%s %s", emoji, friendlyEventName(p.EventType))},
		},
	}

	// Fields block.
	fields := []slackText{
		{Type: "mrkdwn", Text: fmt.Sprintf("*Job:*\n%s", valueOrDash(p.JobAlias))},
		{Type: "mrkdwn", Text: fmt.Sprintf("*Run ID:*\n`%s`", shortID(p.RunID))},
	}
	if p.TaskID.String() != "00000000-0000-0000-0000-000000000000" {
		fields = append(fields, slackText{Type: "mrkdwn", Text: fmt.Sprintf("*Task ID:*\n`%s`", shortID(p.TaskID))})
	}
	fields = append(fields, slackText{Type: "mrkdwn", Text: fmt.Sprintf("*Time:*\n%s", p.Timestamp.Format(time.RFC3339))})

	blocks = append(blocks, slackBlock{Type: "section", Fields: fields})

	// Error block.
	if p.Error != "" {
		errText := p.Error
		if len(errText) > 2000 {
			errText = errText[:2000] + "…"
		}
		blocks = append(blocks, slackBlock{
			Type: "section",
			Text: &slackText{Type: "mrkdwn", Text: fmt.Sprintf("*Error:*\n```%s```", errText)},
		})
	}

	msg.Blocks = blocks
	return msg
}

func eventEmoji(t event.Type) string {
	switch t {
	case event.TypeTaskFailed:
		return "❌"
	case event.TypeRunFailed:
		return "🔴"
	case event.TypeRunTimedOut:
		return "⏱️"
	case event.TypeSLAMissed:
		return "⚠️"
	case event.TypeRunCompleted:
		return "✅"
	case event.TypeTaskSucceeded:
		return "✅"
	default:
		return "📢"
	}
}

func friendlyEventName(t event.Type) string {
	switch t {
	case event.TypeTaskFailed:
		return "Task Failed"
	case event.TypeRunFailed:
		return "Run Failed"
	case event.TypeRunTimedOut:
		return "Run Timed Out"
	case event.TypeSLAMissed:
		return "SLA Missed"
	case event.TypeRunCompleted:
		return "Run Completed"
	case event.TypeTaskSucceeded:
		return "Task Succeeded"
	default:
		return string(t)
	}
}

func valueOrDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func shortID(id uuid.UUID) string {
	s := id.String()
	if len(s) >= 8 {
		return s[:8]
	}
	return s
}
