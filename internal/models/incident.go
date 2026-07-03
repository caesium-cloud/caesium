package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// IncidentStatus enumerates the states of the incident status machine
// (design-agent-in-the-loop.md, Phase 0). The lifecycle is:
//
//	open → triaging → (awaiting_approval ↔ triaging) → remediated | escalated → closed
//
// with suppressed and abandoned as additional terminal dispositions.
type IncidentStatus string

const (
	IncidentStatusOpen             IncidentStatus = "open"
	IncidentStatusTriaging         IncidentStatus = "triaging"
	IncidentStatusAwaitingApproval IncidentStatus = "awaiting_approval"
	IncidentStatusRemediated       IncidentStatus = "remediated"
	IncidentStatusEscalated        IncidentStatus = "escalated"
	IncidentStatusClosed           IncidentStatus = "closed"
	IncidentStatusSuppressed       IncidentStatus = "suppressed"
	IncidentStatusAbandoned        IncidentStatus = "abandoned"
)

// IsTerminal reports whether the status is a terminal incident disposition.
func (s IncidentStatus) IsTerminal() bool {
	switch s {
	case IncidentStatusClosed, IncidentStatusSuppressed, IncidentStatusAbandoned:
		return true
	default:
		return false
	}
}

// Incident records a classified failure that Caesium's incident manager opened
// from the event bus. Incidents are append-mostly, low-volume catalog rows —
// NOT a hot per-run table — so they are not listed in hotPathModels().
type Incident struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	// Namespace is nullable from day one (design Open Question 4). Empty/NULL
	// means the default namespace; a value scopes the incident to a tenant.
	Namespace *string `gorm:"type:text;index" json:"namespace,omitempty"`

	JobID    uuid.UUID  `gorm:"type:uuid;index;not null" json:"job_id"`
	RunID    *uuid.UUID `gorm:"type:uuid;index" json:"run_id,omitempty"`
	TaskID   *uuid.UUID `gorm:"type:uuid;index" json:"task_id,omitempty"`
	TaskName string     `gorm:"type:text" json:"task_name,omitempty"`

	// Class is the deterministic failure_class the classifier assigned.
	Class string `gorm:"type:text;index;not null" json:"class"`
	// Status is the current position in the incident status machine.
	Status IncidentStatus `gorm:"type:text;index;not null" json:"status"`

	// DedupeKey is the stable correlation key (job_id, task_name, failure_class).
	// It is recorded on every incident (open or closed) for history and querying.
	DedupeKey string `gorm:"type:text;index;not null" json:"dedupe_key"`
	// ActiveDedupeKey enforces "at most one non-terminal incident per dedupe key":
	// it holds DedupeKey while the incident is open and is set NULL on any terminal
	// transition. The unique index only constrains non-NULL rows (dqlite/SQLite
	// semantics), so closed incidents never collide — mirroring the nullable
	// unique-index pattern used by JobRun.ReplayFingerprint. Incident-open is an
	// atomic conditional insert on this column.
	ActiveDedupeKey *string `gorm:"type:text;uniqueIndex:idx_incidents_active_dedupe" json:"-"`

	// OccurrenceCount counts distinct failures folded into this incident (the
	// first open is occurrence 1; an independent same-key failure appends one).
	OccurrenceCount int `gorm:"not null;default:1" json:"occurrence_count"`
	// Attempt counts remediation attempts taken against this incident.
	Attempt int `gorm:"not null;default:0" json:"attempt"`
	// BackfillID storm-controls backfill-originated failures so a single backfill
	// does not open one incident per spawned run.
	BackfillID *uuid.UUID `gorm:"type:uuid;index" json:"backfill_id,omitempty"`

	// RemediationTargetRunID is the run whose success closes the incident as
	// remediated. It advances when a new occurrence folds in.
	RemediationTargetRunID *uuid.UUID `gorm:"type:uuid" json:"remediation_target_run_id,omitempty"`

	// LastError is the failing task's error text captured at open, for the feed.
	LastError string `gorm:"type:text" json:"last_error,omitempty"`
	// ResolutionSummary is a short human/agent summary written at close.
	ResolutionSummary string `gorm:"type:text" json:"resolution_summary,omitempty"`
	// Evidence carries classifier evidence (exit code, matched rule) as JSON.
	Evidence datatypes.JSON `gorm:"type:json" json:"evidence,omitempty"`

	OpenedAt  time.Time  `gorm:"not null;index" json:"opened_at"`
	ClosedAt  *time.Time `json:"closed_at,omitempty"`
	CreatedAt time.Time  `gorm:"not null" json:"created_at"`
	UpdatedAt time.Time  `gorm:"not null" json:"updated_at"`
}
