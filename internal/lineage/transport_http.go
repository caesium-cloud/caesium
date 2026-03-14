package lineage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type HTTPTransportConfig struct {
	URL     string
	Headers map[string]string
	Timeout time.Duration
}

type httpTransport struct {
	client  *http.Client
	url     string
	headers map[string]string
}

func NewHTTPTransport(cfg HTTPTransportConfig) Transport {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	return &httpTransport{
		client:  &http.Client{Timeout: timeout},
		url:     strings.TrimRight(cfg.URL, "/"),
		headers: cfg.Headers,
	}
}

func (t *httpTransport) Emit(ctx context.Context, event RunEvent) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("lineage: marshal event: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("lineage: build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("lineage: send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("lineage: backend responded %d", resp.StatusCode)
	}

	return nil
}

func (t *httpTransport) Close() error { return nil }
