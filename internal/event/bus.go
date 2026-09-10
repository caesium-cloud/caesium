package event

import (
	"context"
	"encoding/json"
	"slices"
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
	// TypeDataViolationRecorded is the data-quality sibling of
	// TypeSchemaViolationRecorded: a task's emitted ##caesium::metrics breached
	// a declared assertion but the task did NOT fail (onViolation: warn, a
	// cold-start "seeding" verdict, or — until Stream C wires the breaker —
	// onViolation: hold). Exactly as with schema violations, an onViolation:
	// fail breach fails the task and its task_failed event already carries the
	// violations, so no separate event is emitted for it.
	TypeDataViolationRecorded Type = "data_violation_recorded"
	// TypeDatasetHeld is the data circuit breaker's page-worthy event: a
	// declared dataset broke its contract under onViolation: hold, the
	// producing task SUCCEEDED, and the DATASET is now held — every downstream
	// consumer is admitted straight to skipped until it is released.
	//
	// ALERT-ONCE IS STRUCTURAL: this is emitted by the atomic insert that opens
	// the hold and by nothing else. A repeat breach of an already-held dataset
	// increments the hold's occurrence counter and emits nothing, so a broken
	// hourly job pages once rather than twenty-four times a day. Nothing in the
	// notification layer has to de-duplicate.
	//
	// The payload carries the dataset (namespace, name), the hold id, the
	// violated assertion and the observed/bound/baseline triple, so Stream F's
	// incident entry point needs no second read.
	TypeDatasetHeld Type = "dataset_held"
	// TypeDatasetReleased is emitted when an active hold is closed — either by
	// the holder's next clean producer run (release_reason: clean_run, only for
	// a dataset declared `release: auto`) or by an authenticated human ack
	// through POST /v1/datasets/holds/:id/release (release_reason: manual_ack).
	TypeDatasetReleased Type = "dataset_released"
	// TypeRunHeldUpstream is emitted when the admission gate refuses a run
	// because a dataset the job declares under datasets.consumes is held. The
	// run EXISTS — a row in terminal `skipped` status with its SkipReason and
	// its skipped task rows — so run history explains itself.
	//
	// It defaults to NO-NOTIFY (it is absent from notification.notifiableTypes):
	// one broken dataset can skip many downstream runs, and paging per skipped
	// run is exactly the alert storm dataset_held's alert-once design avoids.
	// It is still persisted and streamed, so the Console and `caesium why` see
	// it.
	TypeRunHeldUpstream Type = "run_held_upstream"
	// TypeContractBreakDeclared is emitted when an operator intentionally
	// acknowledges a breaking cross-job data contract for a bounded
	// deprecation window.
	TypeContractBreakDeclared Type = "contract_break_declared"

	// Incident lifecycle events (agent-in-the-loop D2). Emitted on the existing
	// /events stream so the Console incidents surface (Stream U) can live-update
	// the feed, timeline, and approval inbox without polling.
	//
	// TypeIncidentOpened is the FIRST lifecycle event for any incident,
	// published by internal/incident.Subscriber.handleFailure the moment
	// Store.OpenOrAppend reports OutcomeOpened (a brand-new incident row, not an
	// occurrence folded into an existing one — see OutcomeAppended). Without it
	// a consumer of the event stream could see every later lifecycle event
	// (approval_requested, incident_status_changed, ...) but never the
	// incident's own start (#419). The payload carries incident_id, job_id,
	// run_id/task_id/task_name when known, the classifier's class, status, and
	// dedupe_key — the same correlation shape as its siblings below.
	TypeIncidentOpened        Type = "incident_opened"
	TypeIncidentStatusChanged Type = "incident_status_changed"
	TypeAgentActionRecorded   Type = "agent_action_recorded"
	TypeApprovalRequested     Type = "approval_requested"
	// TypeAgentActionExecuted is emitted when an approved tier-3 action actually
	// RUNS (trust-the-substrate C8). It is deliberately distinct from
	// TypeAgentActionRecorded, which the approvals controller emits at DECISION
	// time and which therefore says nothing about whether the action executed or
	// what it did. This one carries the decider, the action type/tier, and the
	// result summary, so "approved by X at T, executed at T'" is reconstructable
	// from the event stream alone.
	TypeAgentActionExecuted Type = "agent_action_executed"
	// TypeIncidentEscalated is emitted when a remediation ESCALATES — an agent or
	// a deterministic rule handed the incident to a human, either directly
	// (the `escalate` action) or because an approved change could not be applied
	// where it was approved (a git-synced job's jobdef patch, which degrades to an
	// escalation carrying the rendered diff).
	//
	// It exists because escalation must be DELIVERED, not merely recorded. An
	// escalation that only writes an AgentAction row and a log line contacts
	// nobody, which is the one outcome an escalation cannot have. Routing it as an
	// event puts it through the ordinary NotificationPolicy → channel machinery,
	// so a team pages or Slacks on it with no new plumbing, and it stays queryable
	// from /v1/events afterwards. The payload carries the incident, the requested
	// channel, and the rendered summary/diff.
	TypeIncidentEscalated Type = "incident_escalated"
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
		found := slices.Contains(filter.Types, e.Type)
		if !found {
			return false
		}
	}
	return true
}
