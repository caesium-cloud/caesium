package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// DefaultTriageOnlyProfileName is the shipped zero-risk default AgentProfile:
// tier 0 (deterministic rules only) plus escalate, so teams can adopt
// agent-in-the-loop remediation incrementally without granting any autonomous
// action. It is a built-in name: it is seeded into, and migrated to, the
// cluster-shared namespace "*", so every job namespace may reference it;
// server-side lint/apply treats it as resolvable while no row exists, and once
// a row exists it is checked like any other profile (job's namespace or "*").
// See docs/design/agent-in-the-loop.md.
const DefaultTriageOnlyProfileName = "triage-only"

// AgentProfile is the server-side resource declaring how a remediation agent
// runs: its container image/engine, resource limits, model-credential
// secret:// references, session budgets, and the default playbook. It is
// referenced by a job's metadata.remediation block. Low-volume catalog row.
type AgentProfile struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	// Namespace is the tenancy namespace; "*" (ClusterNamespace) marks a
	// cluster-shared profile. It is create-only.
	Namespace string `gorm:"type:text;not null;default:'default';index" json:"namespace"`

	Name   string     `gorm:"uniqueIndex;not null" json:"name"`
	Image  string     `gorm:"type:text;not null" json:"image"`
	Engine AtomEngine `gorm:"type:text;not null;default:'docker'" json:"engine"`

	// Limits holds resource limits (cpu/memory/wall-clock) as JSON.
	Limits datatypes.JSON `gorm:"type:json" json:"limits,omitempty"`
	// SecretRefs holds secret:// references (e.g. the model API key) as JSON.
	// Never resolved values — only the references.
	SecretRefs datatypes.JSON `gorm:"type:json" json:"secret_refs,omitempty"`
	// Budgets holds per-session budget defaults (max actions, token/cost caps).
	Budgets datatypes.JSON `gorm:"type:json" json:"budgets,omitempty"`
	// Playbook holds the default action-catalog policy for jobs using this profile.
	Playbook datatypes.JSON `gorm:"type:json" json:"playbook,omitempty"`

	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
	CreatedAt time.Time      `gorm:"not null" json:"created_at"`
	UpdatedAt time.Time      `gorm:"not null" json:"updated_at"`
}
