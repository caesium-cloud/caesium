package models

import (
	"time"

	"github.com/google/uuid"
)

// ApprovalDecision enumerates the outcome of a tier-3 approval request.
type ApprovalDecision string

const (
	ApprovalDecisionPending  ApprovalDecision = "pending"
	ApprovalDecisionApproved ApprovalDecision = "approved"
	ApprovalDecisionRejected ApprovalDecision = "rejected"
	ApprovalDecisionExpired  ApprovalDecision = "expired"
)

// ApprovalRequest is created by every tier-3 action and resolved by a human
// operator. Tier-3 actions are NEVER auto-executed regardless of config, so an
// approval always terminates at a human decision. Low-volume catalog row.
type ApprovalRequest struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	// Namespace is nullable from day one (design Open Question 4).
	Namespace *string `gorm:"type:text;index" json:"namespace,omitempty"`

	// IncidentID lets the operator read API filter approvals by incident without
	// a join through the action.
	IncidentID uuid.UUID `gorm:"type:uuid;index;not null" json:"incident_id"`

	ActionID uuid.UUID   `gorm:"type:uuid;index;not null" json:"action_id"`
	Action   AgentAction `gorm:"constraint:OnDelete:CASCADE" json:"-"`

	// ApproversHint is a free-text hint for who should decide (role/channel).
	ApproversHint string           `gorm:"type:text" json:"approvers_hint,omitempty"`
	Decision      ApprovalDecision `gorm:"type:text;index;not null;default:'pending'" json:"decision"`
	// Decider records the operator identity that resolved the request.
	Decider string `gorm:"type:text" json:"decider,omitempty"`
	Reason  string `gorm:"type:text" json:"reason,omitempty"`

	ExpiresAt  *time.Time `gorm:"index" json:"expires_at,omitempty"`
	DecidedAt  *time.Time `json:"decided_at,omitempty"`
	CreatedAt  time.Time  `gorm:"not null" json:"created_at"`
	UpdatedAt  time.Time  `gorm:"not null" json:"updated_at"`
}
