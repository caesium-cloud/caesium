package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
)

// webhookConfig is the expected JSON shape of a webhook channel's Config.
type webhookConfig struct {
	URL     string            `json:"url"`
	Method  string            `json:"method,omitempty"`  // defaults to POST
	Headers map[string]string `json:"headers,omitempty"`
	Timeout string            `json:"timeout,omitempty"` // Go duration string, defaults to 10s
}

// WebhookSender sends notifications as HTTP requests.
type WebhookSender struct {
	client *http.Client
}

// NewWebhookSender creates a webhook notification sender.
func NewWebhookSender() *WebhookSender {
	return &WebhookSender{
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (s *WebhookSender) Send(ctx context.Context, ch models.NotificationChannel, payload Payload) error {
	var cfg webhookConfig
	if err := json.Unmarshal(ch.Config, &cfg); err != nil {
		return fmt.Errorf("webhook: invalid channel config: %w", err)
	}

	if cfg.URL == "" {
		return fmt.Errorf("webhook: url is required in channel %q config", ch.Name)
	}

	method := strings.ToUpper(cfg.Method)
	if method == "" {
		method = http.MethodPost
	}

	timeout := 10 * time.Second
	if cfg.Timeout != "" {
		if d, err := time.ParseDuration(cfg.Timeout); err == nil && d > 0 {
			timeout = d
		}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("webhook: marshal payload: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, method, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook: build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "caesium-notification/1.0")
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	log.Warn("webhook: non-2xx response",
		"channel", ch.Name,
		"host",    redactURL(cfg.URL),
		"status",  resp.StatusCode,
	)
	return fmt.Errorf("webhook: server responded %d", resp.StatusCode)
}

func redactURL(u string) string {
	if u == "" {
		return ""
	}
	return "[redacted]"
}
