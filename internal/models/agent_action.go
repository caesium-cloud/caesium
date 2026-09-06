package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// AgentActionStatus enumerates an action's lifecycle in the audit spine.
type AgentActionStatus string

const (
	AgentActionStatusProposed AgentActionStatus = "proposed"
	AgentActionStatusApproved AgentActionStatus = "approved"
	AgentActionStatusRejected AgentActionStatus = "rejected"
	// AgentActionStatusExecuting is the CLAIM an approved action holds while it
	// is being dispatched. It exists so dispatch is once-only: the executor moves
	// approved → executing with a conditional UPDATE, and a second attempt (the
	// redrive sweeper racing the synchronous post-decision execute, or two
	// operators' requests interleaving) matches zero rows and does nothing.
	//
	// A row left `executing` by a process death is a deliberately VISIBLE stuck
	// state, not an invisible one: it is never auto-redriven, because re-running a
	// half-applied tier-3 mutation (a jobdef patch, a skipped task) unattended is
	// worse than surfacing it for a human. See ApprovalRedriver.
	AgentActionStatusExecuting AgentActionStatus = "executing"
	AgentActionStatusExecuted  AgentActionStatus = "executed"
	AgentActionStatusFailed    AgentActionStatus = "failed"
)

// AgentActionActor identifies who originated an action row.
type AgentActionActor string

const (
	// ActorPolicy is a deterministic server-side rule (Phase 0) — no container.
	AgentActionActorPolicy AgentActionActor = "policy"
	// ActorAgent is the container-native agent (Phase 1+).
	AgentActionActorAgent AgentActionActor = "agent"
	// ActorHuman is an operator action.
	AgentActionActorHuman AgentActionActor = "human"
)

// AgentAction is the audit spine: every remediation attempt (whether taken by a
// deterministic policy rule, the container agent, or a human) is recorded as a
// row. The incident reference is NON-NULLABLE so the timeline reconstructs for
// actor=policy|human rows that have no session; the session reference is
// nullable. Append-only, low-volume.
type AgentAction struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	// Namespace is nullable from day one (design Open Question 4).
	Namespace *string `gorm:"type:text;index" json:"namespace,omitempty"`

	// IncidentID is non-nullable — the audit spine always ties to an incident.
	IncidentID uuid.UUID `gorm:"type:uuid;index;not null" json:"incident_id"`
	Incident   Incident  `gorm:"constraint:OnDelete:CASCADE" json:"-"`

	// SessionID is nullable: policy/human rows have no agent session.
	SessionID *uuid.UUID    `gorm:"type:uuid;index" json:"session_id,omitempty"`
	Session   *AgentSession `gorm:"constraint:OnDelete:SET NULL" json:"-"`

	Type   string            `gorm:"type:text;index;not null" json:"type"`
	Params datatypes.JSON    `gorm:"type:json" json:"params,omitempty"`
	Tier   int               `gorm:"not null;default:0" json:"tier"`
	Status AgentActionStatus `gorm:"type:text;index;not null" json:"status"`
	Result datatypes.JSON    `gorm:"type:json" json:"result,omitempty"`
	Actor  AgentActionActor  `gorm:"type:text;index;not null" json:"actor"`

	CreatedAt time.Time `gorm:"not null;index" json:"created_at"`
	UpdatedAt time.Time `gorm:"not null" json:"updated_at"`
}
