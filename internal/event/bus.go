package event

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
)

const defaultSubscriberBuffer = 1000

// EventType represents the type of event.
type Type string

const (
	TypeJobCreated             Type = "job_created"
	TypeJobDeleted             Type = "job_deleted"
	TypeRunStarted             Type = "run_started"
	TypeRunCompleted           Type = "run_completed"
	TypeRunFailed              Type = "run_failed"
	TypeRunCancelled           Type = "run_cancelled"
	TypeRunTerminal            Type = "run_terminal"
	TypeTaskStarted            Type = "task_started"
	TypeTaskSucceeded          Type = "task_succeeded"
	TypeTaskFailed             Type = "task_failed"
	TypeTaskSkipped            Type = "task_skipped"
	TypeTaskRetrying           Type = "task_retrying"
	TypeTaskReady              Type = "task_ready"
	TypeTaskCached             Type = "task_cached"
	TypeTaskClaimed            Type = "task_claimed"
	TypeTaskLeaseExpired       Type = "task_lease_expired"
	TypeLogChunk               Type = "log_chunk"
	TypeJobPaused              Type = "job_paused"
	TypeJobUnpaused            Type = "job_unpaused"
	TypeBackfillStarted        Type = "backfill_started"
	TypeBackfillComplete       Type = "backfill_completed"
	TypeBackfillFailed         Type = "backfill_failed"
	TypeBackfillCancelled      Type = "backfill_cancelled"
	TypeRunRetried             Type = "run_retried"
	TypeRunTimedOut            Type = "run_timed_out"
	TypeSLAMissed              Type = "sla_missed"
	TypeFreshnessViolated      Type = "freshness_violated"
	TypeDatasetFreshnessAtRisk Type = "dataset_freshness_at_risk"
	// TypeDatasetAdvanced fires after a dataset's watermark is advanced or
	// verify-refreshed — by the run-completion capturer or the arrival observer —
	// carrying {namespace, name} in its payload. The freshness evaluator
	// subscribes to it to reactively re-derive downstream consumers off
	// POST-advance state. Reacting to run_completed instead would race the
	// capturer's own Advance (the bus fans out to subscribers unordered), so the
	// evaluator could read pre-advance state and derive a redundant producer run.
	TypeDatasetAdvanced Type = "dataset_advanced"
	// TypeSchemaViolationRecorded is emitted when a task's output violates its
	// declared schema in "warn" mode — the task does NOT fail, so the incident
	// manager would otherwise never see the violation. In "fail" mode the task
	// failure already carries the violations, so no separate event is emitted.
	TypeSchemaViolationRecorded Type = "schema_violation_recorded"
	// TypeContractBreakDeclared is emitted when an operator intentionally
	// acknowledges a breaking cross-job data contract for a bounded
	// deprecation window.
	TypeContractBreakDeclared Type = "contract_break_declared"

	// Incident lifecycle events (agent-in-the-loop D2). Emitted on the existing
	// /events stream so the Console incidents surface (Stream U) can live-update
	// the feed, timeline, and approval inbox without polling.
	TypeIncidentOpened        Type = "incident_opened"
	TypeIncidentStatusChanged Type = "incident_status_changed"
	TypeAgentActionRecorded   Type = "agent_action_recorded"
	TypeApprovalRequested     Type = "approval_requested"
)

// Event represents a system event.
type Event struct {
	Sequence   uint64          `json:"sequence,omitempty"`
	Type       Type            `json:"type"`
	JobID      uuid.UUID       `json:"job_id,omitempty"`
	RunID      uuid.UUID       `json:"run_id,omitempty"`
	TaskID     uuid.UUID       `json:"task_id,omitempty"`
	Timestamp  time.Time       `json:"timestamp"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	Quarantine bool            `json:"quarantine,omitempty"`
}

// Filter defines criteria for receiving events.
type Filter struct {
	JobID             uuid.UUID
	RunID             uuid.UUID
	Types             []Type
	IncludeQuarantine bool
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
				metrics.EventBusDroppedTotal.WithLabelValues(string(e.Type)).Inc()
				log.Warn("event bus subscriber buffer full; dropping event",
					"type", e.Type,
					"sequence", e.Sequence,
					"job_id", e.JobID,
					"run_id", e.RunID,
				)
			}
		}
	}
}

func (b *bus) Subscribe(ctx context.Context, filter Filter) (<-chan Event, error) {
	ch := make(chan Event, defaultSubscriberBuffer)

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
	if e.Quarantine && !filter.IncludeQuarantine {
		return false
	}
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
