package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// AgentSessionState enumerates the terminal-tracked lifecycle of an agent
// container session.
type AgentSessionState string

const (
	AgentSessionStatePending   AgentSessionState = "pending"
	AgentSessionStateRunning   AgentSessionState = "running"
	AgentSessionStateSucceeded AgentSessionState = "succeeded"
	AgentSessionStateFailed    AgentSessionState = "failed"
	AgentSessionStateTimedOut  AgentSessionState = "timed_out"
	AgentSessionStateCancelled AgentSessionState = "cancelled"
)

// AgentSession records a single agent container run launched through the
// existing atom.Engine to triage an incident. It is DELIBERATELY NOT a JobRun /
// TaskRun: a session materialized as a run would pollute the quarantine-filtered
// run statistics and feed its own exhaust back into the incident bus. Low-volume.
type AgentSession struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	// Namespace is nullable from day one (design Open Question 4).
	Namespace *string `gorm:"type:text;index" json:"namespace,omitempty"`

	IncidentID uuid.UUID `gorm:"type:uuid;index;not null" json:"incident_id"`
	Incident   Incident  `gorm:"constraint:OnDelete:CASCADE" json:"-"`

	ProfileID *uuid.UUID    `gorm:"type:uuid;index" json:"profile_id,omitempty"`
	Profile   *AgentProfile `gorm:"constraint:OnDelete:SET NULL" json:"-"`

	Engine AtomEngine `gorm:"type:text" json:"engine,omitempty"`
	// AtomID / ContainerID identify the launched container/atom for the UI and
	// for stop/cleanup.
	AtomID      *uuid.UUID `gorm:"type:uuid;index" json:"atom_id,omitempty"`
	ContainerID string     `gorm:"type:text" json:"container_id,omitempty"`

	// TokenID is the scoped, short-lived agent credential bound to this session.
	TokenID *uuid.UUID `gorm:"type:uuid;index" json:"token_id,omitempty"`

	State AgentSessionState `gorm:"type:text;index;not null;default:'pending'" json:"state"`
	// SessionLog is the persisted agent-container log for the UI timeline.
	SessionLog string `gorm:"type:text" json:"session_log,omitempty"`

	// Budget counters. These are best-effort accounting the executor increments.
	ActionsUsed int `gorm:"not null;default:0" json:"actions_used"`
	TokensUsed  int `gorm:"not null;default:0" json:"tokens_used"`

	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	CreatedAt   time.Time  `gorm:"not null" json:"created_at"`
	UpdatedAt   time.Time  `gorm:"not null" json:"updated_at"`
	// Extra carries additional session metadata (e.g. frozen allowlist) as JSON.
	Extra datatypes.JSON `gorm:"type:json" json:"extra,omitempty"`
}
