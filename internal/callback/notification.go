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

var _ DetailedHandler = (*NotificationHandler)(nil)

// NewNotificationHandler constructs a notification handler with the provided client.
func NewNotificationHandler(client *http.Client) *NotificationHandler {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &NotificationHandler{client: client}
}

// Handle sends a POST request containing the run metadata.
func (h *NotificationHandler) Handle(ctx context.Context, cfgRaw json.RawMessage, meta Metadata) error {
	_, err := h.HandleWithResult(ctx, cfgRaw, meta)
	return err
}

// HandleWithResult sends the notification and reports the transport detail the
// dispatcher persists on the CallbackRun: the status the target answered with,
// the (bounded) body it answered with, and the number of requests actually
// sent. A failure that never reached the wire — a bad configuration, an
// unbuildable request — reports zero attempts and no status, which is how an
// operator tells "we never called you" from "you answered 500".
func (h *NotificationHandler) HandleWithResult(ctx context.Context, cfgRaw json.RawMessage, meta Metadata) (Result, error) {
	var result Result

	var cfg NotificationConfig
	if len(cfgRaw) > 0 {
		if err := json.Unmarshal(cfgRaw, &cfg); err != nil {
			return result, fmt.Errorf("parse configuration: %w", err)
		}
	}

	target := strings.TrimSpace(cfg.URL)
	if target == "" {
		target = strings.TrimSpace(cfg.Webhook)
	}
	if target == "" {
		return result, errors.New("notification requires url or webhook_url")
	}

	body, err := json.Marshal(meta)
	if err != nil {
		return result, fmt.Errorf("encode payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return result, fmt.Errorf("build request: %w", err)
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

	result.Attempts = 1
	resp, err := h.client.Do(req)
	if err != nil {
		// No response at all: HTTPStatus stays 0, which is the marker for a
		// transport failure rather than a rejection by the target.
		return result, fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read the body on every outcome, not just the failing one: it is the
	// evidence an operator needs on the run detail page, and draining it lets
	// the connection be reused.
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
	result.HTTPStatus = resp.StatusCode
	result.ResponseBody = strings.TrimSpace(string(respBody))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return result, fmt.Errorf("webhook responded %d: %s", resp.StatusCode, result.ResponseBody)
	}

	return result, nil
}
