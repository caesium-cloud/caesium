package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// RemediationTimerStatus enumerates the lifecycle of a durable timer.
type RemediationTimerStatus string

const (
	// Pending: not yet due / not yet fired.
	RemediationTimerStatusPending RemediationTimerStatus = "pending"
	// Fired: the sweeper fired the timer's action.
	RemediationTimerStatusFired RemediationTimerStatus = "fired"
	// Cancelled: the owning incident reached a terminal transition or a human
	// took over, so the timer must never fire.
	RemediationTimerStatusCancelled RemediationTimerStatus = "cancelled"
)

// RemediationTimer is a durable snooze/retry row backing snooze_retry actions.
// Every existing delay in Caesium is an in-process time.NewTimer lost on
// restart/failover; this row survives failover so a leader-gated sweeper can
// fire due timers. Timers are OWNED BY THEIR INCIDENT and cancelled on any
// terminal transition or human take-over. Low-volume catalog row.
type RemediationTimer struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	// Namespace is nullable from day one (design Open Question 4).
	Namespace *string `gorm:"type:text;index" json:"namespace,omitempty"`

	IncidentID uuid.UUID `gorm:"type:uuid;index;not null" json:"incident_id"`
	Incident   Incident  `gorm:"constraint:OnDelete:CASCADE" json:"-"`

	// ActionID optionally links the timer to the AgentAction that scheduled it.
	ActionID *uuid.UUID `gorm:"type:uuid;index" json:"action_id,omitempty"`

	// Kind identifies what the sweeper should do when the timer fires
	// (e.g. "snooze_retry").
	Kind string `gorm:"type:text;not null" json:"kind"`
	// Payload carries the fire-time parameters (e.g. target run id) as JSON.
	Payload datatypes.JSON `gorm:"type:json" json:"payload,omitempty"`

	Status RemediationTimerStatus `gorm:"type:text;index;not null;default:'pending'" json:"status"`
	// FireAt is when the timer becomes due. Indexed so the sweeper's due-scan
	// (status = pending AND fire_at <= now) is cheap.
	FireAt  time.Time  `gorm:"not null;index" json:"fire_at"`
	FiredAt *time.Time `json:"fired_at,omitempty"`

	CreatedAt time.Time `gorm:"not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"not null" json:"updated_at"`
}
