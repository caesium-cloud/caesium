package callback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// NotificationConfig describes the webhook target.
type NotificationConfig struct {
	URL       string            `json:"url"`
	Webhook   string            `json:"webhook_url"`
	Headers   map[string]string `json:"headers,omitempty"`
	UserAgent string            `json:"user_agent,omitempty"`
}

// NotificationHandler posts run metadata to a webhook endpoint.
type NotificationHandler struct {
	client *http.Client
}

// NewNotificationHandler constructs a notification handler with the provided client.
func NewNotificationHandler(client *http.Client) *NotificationHandler {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &NotificationHandler{client: client}
}

// Handle sends a POST request containing the run metadata.
func (h *NotificationHandler) Handle(ctx context.Context, cfgRaw json.RawMessage, meta Metadata) error {
	var cfg NotificationConfig
	if len(cfgRaw) > 0 {
		if err := json.Unmarshal(cfgRaw, &cfg); err != nil {
			return fmt.Errorf("parse configuration: %w", err)
		}
	}

	target := strings.TrimSpace(cfg.URL)
	if target == "" {
		target = strings.TrimSpace(cfg.Webhook)
	}
	if target == "" {
		return errors.New("notification requires url or webhook_url")
	}

	body, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if ua := strings.TrimSpace(cfg.UserAgent); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	for k, v := range cfg.Headers {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		req.Header.Set(k, v)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("webhook responded %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return nil
}
