package event

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"
)

// EventType represents the type of event.
type Type string

const (
	TypeJobCreated    Type = "job_created"
	TypeJobDeleted    Type = "job_deleted"
	TypeRunStarted    Type = "run_started"
	TypeRunCompleted  Type = "run_completed"
	TypeRunFailed     Type = "run_failed"
	TypeTaskStarted   Type = "task_started"
	TypeTaskSucceeded Type = "task_succeeded"
	TypeTaskFailed    Type = "task_failed"
	TypeTaskSkipped   Type = "task_skipped"
	TypeLogChunk      Type = "log_chunk"
)

// Event represents a system event.
type Event struct {
	Type      Type            `json:"type"`
	JobID     uuid.UUID       `json:"job_id,omitempty"`
	RunID     uuid.UUID       `json:"run_id,omitempty"`
	TaskID    uuid.UUID       `json:"task_id,omitempty"`
	Timestamp time.Time       `json:"timestamp"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// Filter defines criteria for receiving events.
type Filter struct {
	JobID uuid.UUID
	RunID uuid.UUID
	Types []Type
}

// Bus defines the event bus interface.
type Bus interface {
	Publish(e Event)
	Subscribe(ctx context.Context, filter Filter) (<-chan Event, error)
}

type bus struct {
	subscribers map[chan Event]Filter
	mu          sync.RWMutex
}

// New creates a new event bus.
func New() Bus {
	return &bus{
		subscribers: make(map[chan Event]Filter),
	}
}

func (b *bus) Publish(e Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for ch, filter := range b.subscribers {
		if b.matches(filter, e) {
			select {
			case ch <- e:
			default:
				// Drop event if channel is full to prevent blocking
			}
		}
	}
}

func (b *bus) Subscribe(ctx context.Context, filter Filter) (<-chan Event, error) {
	ch := make(chan Event, 100)

	b.mu.Lock()
	b.subscribers[ch] = filter
	b.mu.Unlock()

	go func() {
		<-ctx.Done()
		b.mu.Lock()
		delete(b.subscribers, ch)
		close(ch)
		b.mu.Unlock()
	}()

	return ch, nil
}

func (b *bus) matches(filter Filter, e Event) bool {
	if filter.JobID != uuid.Nil && filter.JobID != e.JobID {
		return false
	}
	if filter.RunID != uuid.Nil && filter.RunID != e.RunID {
		return false
	}
	if len(filter.Types) > 0 {
		found := false
		for _, t := range filter.Types {
			if t == e.Type {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
