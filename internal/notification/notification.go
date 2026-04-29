package notification

import (
	"context"
	"encoding/json"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

// Sender delivers a notification payload to a specific channel type.
// The channel's Config field contains the per-channel configuration
// (URL, credentials, routing keys, etc.) as JSON.
type Sender interface {
	Send(ctx context.Context, channel models.NotificationChannel, payload Payload) error
}

// Payload is the notification content delivered to channels.
type Payload struct {
	EventType  event.Type        `json:"event_type"`
	JobID      uuid.UUID         `json:"job_id"`
	JobAlias   string            `json:"job_alias,omitempty"`
	JobLabels  map[string]string `json:"job_labels,omitempty"`
	RunID      uuid.UUID         `json:"run_id"`
	TaskID     uuid.UUID         `json:"task_id,omitempty"`
	Error      string            `json:"error,omitempty"`
	Timestamp  time.Time         `json:"timestamp"`
	RawPayload json.RawMessage   `json:"payload,omitempty"`
}

// PolicyFilter defines optional filters on a notification policy.
type PolicyFilter struct {
	JobIDs   []uuid.UUID `json:"job_ids,omitempty"`
	JobAlias string      `json:"job_alias,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
}
