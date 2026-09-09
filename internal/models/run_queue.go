package models

import (
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// Claim states of a queued run, as reported by the queue view.
//
// A queued run is claimed by exactly one dequeuer at a time, and that claim is
// a LEASE: the dequeuer stamps `claimed_by`/`claimed_at`, then starts or
// releases the row within the same drain. A claim still standing after the
// lease expires means its claimer died mid-drain, and the leader's reaper will
// release the row on its next pass — until then the row is stuck, so the queue
// view says so instead of hiding it.
const (
	// RunQueueClaimStatePending is an unclaimed row: it is genuinely waiting
	// for a run slot.
	RunQueueClaimStatePending = "pending"
	// RunQueueClaimStateClaimed is a live claim — a dequeuer is starting the
	// row right now. Normally a sub-second state.
	RunQueueClaimStateClaimed = "claimed"
	// RunQueueClaimStateStale is a claim past its lease: nobody is working the
	// row, and it waits for the reaper rather than for capacity.
	RunQueueClaimStateStale = "stale"
)

// DefaultRunQueueClaimStaleAfter is the claim lease used when
// CAESIUM_RUN_QUEUE_CLAIM_STALE_AFTER is unset or non-positive. The reaper and
// the queue view share it so a row can never be stale to one and live to the
// other.
const DefaultRunQueueClaimStaleAfter = 2 * time.Minute

// RunQueueStaleCutoff is the instant before which a claim counts as expired.
func RunQueueStaleCutoff(now time.Time, staleAfter time.Duration) time.Time {
	if staleAfter <= 0 {
		staleAfter = DefaultRunQueueClaimStaleAfter
	}
	return now.UTC().Add(-staleAfter)
}

func (RunQueue) TableName() string { return "run_queue" }

type RunQueue struct {
	ID        uuid.UUID      `gorm:"type:uuid;primaryKey" json:"id"`
	JobID     uuid.UUID      `gorm:"type:uuid;not null;index:idx_run_queue_job_priority_created,priority:1" json:"job_id"`
	Job       Job            `gorm:"constraint:OnDelete:CASCADE" json:"-"`
	Params    datatypes.JSON `gorm:"type:json" json:"params,omitempty"`
	Priority  int            `gorm:"not null;default:2;index:idx_run_queue_job_priority_created,priority:2,sort:desc" json:"priority"`
	ClaimedBy string         `gorm:"type:text;not null;default:'';index" json:"claimed_by"`
	ClaimedAt *time.Time     `gorm:"index" json:"claimed_at,omitempty"`
	CreatedAt time.Time      `gorm:"not null;index:idx_run_queue_job_priority_created,priority:3,sort:asc" json:"created_at"`
}

// ClaimState classifies the row's claim against staleCutoff, which callers get
// from RunQueueStaleCutoff. The predicate is character-for-character the
// reaper's own (`claimed_at IS NULL OR claimed_at < cutoff`), so the view and
// the reaper cannot disagree about which rows are stuck.
func (r RunQueue) ClaimState(staleCutoff time.Time) string {
	if strings.TrimSpace(r.ClaimedBy) == "" {
		return RunQueueClaimStatePending
	}
	if r.ClaimedAt == nil || r.ClaimedAt.Before(staleCutoff) {
		return RunQueueClaimStateStale
	}
	return RunQueueClaimStateClaimed
}
