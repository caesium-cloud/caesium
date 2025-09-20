package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Atom represents an atom resource.
type Atom struct {
	ID        string    `json:"id"`
	Engine    string    `json:"engine"`
	Image     string    `json:"image"`
	Command   []string  `json:"command"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// UnmarshalJSON converts the stored command string to a slice.
func (a *Atom) UnmarshalJSON(data []byte) error {
	type alias struct {
		ID        string    `json:"id"`
		Engine    string    `json:"engine"`
		Image     string    `json:"image"`
		Command   string    `json:"command"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
	}

	var raw alias
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	a.ID = raw.ID
	a.Engine = raw.Engine
	a.Image = raw.Image
	a.CreatedAt = raw.CreatedAt
	a.UpdatedAt = raw.UpdatedAt

	a.Command = nil
	if strings.TrimSpace(raw.Command) != "" {
		var cmd []string
		if err := json.Unmarshal([]byte(raw.Command), &cmd); err == nil {
			a.Command = cmd
		}
	}

	return nil
}

// AtomsResponse wraps the atom list payload.
type AtomsResponse []Atom

// AtomsService exposes atom-related operations.
type AtomsService struct {
	client *Client
}

// List fetches atoms with optional filters.
func (s *AtomsService) List(ctx context.Context, params url.Values) (AtomsResponse, error) {
	endpoint := s.client.resolve("/v1/atoms", params.Encode())

	var payload AtomsResponse
	if err := s.client.do(ctx, http.MethodGet, endpoint, &payload); err != nil {
		return nil, fmt.Errorf("list atoms: %w", err)
	}

	return payload, nil
}
