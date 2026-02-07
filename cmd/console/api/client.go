package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/caesium-cloud/caesium/cmd/console/config"
)

// Client wraps HTTP interaction with the Caesium REST API.
type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
}

// New constructs a client from the provided configuration.
func New(cfg *config.Config) *Client {
	httpClient := &http.Client{Timeout: cfg.HTTPTimeout}

	return &Client{
		baseURL:    cfg.BaseURL,
		httpClient: httpClient,
	}
}

func (c *Client) resolve(path string, queries ...string) string {
	raw := strings.TrimSuffix(c.baseURL.String(), "/") + path
	filtered := make([]string, 0, len(queries))
	for _, q := range queries {
		q = strings.Trim(q, "?& ")
		if q != "" {
			filtered = append(filtered, q)
		}
	}

	if len(filtered) == 0 {
		return raw
	}

	return raw + "?" + strings.Join(filtered, "&")
}

func decodeBody(body io.ReadCloser, target any) error {
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()

	decodeErr := decoder.Decode(target)
	closeErr := body.Close()
	if decodeErr != nil {
		if closeErr != nil {
			return errors.Join(decodeErr, closeErr)
		}
		return decodeErr
	}
	return closeErr
}

func (c *Client) do(ctx context.Context, method, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, method, path, nil)
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}

	if resp.StatusCode >= http.StatusBadRequest {
		errStatus := fmt.Errorf("request failed: %s", resp.Status)
		if err := resp.Body.Close(); err != nil {
			return errors.Join(errStatus, err)
		}
		return errStatus
	}

	if v == nil {
		return resp.Body.Close()
	}

	return decodeBody(resp.Body, v)
}

// Jobs exposes job-related API helpers.
func (c *Client) Jobs() *JobsService {
	return &JobsService{client: c}
}

// Triggers exposes trigger-related API helpers.
func (c *Client) Triggers() *TriggersService {
	return &TriggersService{client: c}
}

// Atoms exposes atom-related API helpers.
func (c *Client) Atoms() *AtomsService {
	return &AtomsService{client: c}
}

// Runs exposes run history API helpers.
func (c *Client) Runs() *RunsService {
	return &RunsService{client: c}
}

type healthResponse struct {
	Status string `json:"status"`
}

// Ping verifies the API health endpoint responds with a healthy status.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.resolve("/health"), nil)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		errStatus := fmt.Errorf("health check failed: request failed: %s", resp.Status)
		if closeErr := resp.Body.Close(); closeErr != nil {
			return errors.Join(errStatus, closeErr)
		}
		return errStatus
	}

	// Keep health parsing permissive so extra payload fields don't break diagnostics.
	decoder := json.NewDecoder(resp.Body)
	var payload healthResponse
	decodeErr := decoder.Decode(&payload)
	closeErr := resp.Body.Close()
	if decodeErr != nil {
		if closeErr != nil {
			return fmt.Errorf("health check failed: %w", errors.Join(decodeErr, closeErr))
		}
		return fmt.Errorf("health check failed: %w", decodeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("health check failed: %w", closeErr)
	}

	if strings.ToLower(strings.TrimSpace(payload.Status)) != "healthy" {
		return fmt.Errorf("health check failed: status=%q", payload.Status)
	}
	return nil
}
