package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Trigger represents a trigger resource in the API response.
type Trigger struct {
	ID            string    `json:"id"`
	Alias         string    `json:"alias"`
	Type          string    `json:"type"`
	Configuration string    `json:"configuration"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// TriggersResponse wraps the trigger list payload.
type TriggersResponse []Trigger

// TriggersService exposes trigger-related operations.
type TriggersService struct {
	client *Client
}

// List fetches triggers with optional filters.
func (s *TriggersService) List(ctx context.Context, params url.Values) (TriggersResponse, error) {
	endpoint := s.client.resolve("/v1/triggers", params.Encode())

	var payload TriggersResponse
	if err := s.client.do(ctx, http.MethodGet, endpoint, &payload); err != nil {
		return nil, fmt.Errorf("list triggers: %w", err)
	}

	return payload, nil
}
