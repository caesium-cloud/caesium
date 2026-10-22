package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// Role defines the RBAC role hierarchy.
// admin > operator > runner > viewer
type Role string

const (
	RoleAdmin    Role = "admin"
	RoleOperator Role = "operator"
	RoleRunner   Role = "runner"
	RoleViewer   Role = "viewer"
)

// RoleLevel returns the numeric privilege level for a role.
// Higher values indicate more privileges.
func RoleLevel(r Role) int {
	switch r {
	case RoleAdmin:
		return 40
	case RoleOperator:
		return 30
	case RoleRunner:
		return 20
	case RoleViewer:
		return 10
	default:
		return 0
	}
}

// ValidRole returns true if r is a recognised role string.
func ValidRole(r string) bool {
	switch Role(r) {
	case RoleAdmin, RoleOperator, RoleRunner, RoleViewer:
		return true
	}
	return false
}

// APIKey represents an API key stored in the database.
// The plaintext key is never persisted — only a versioned stored hash.
//
// Namespaces is the JSON list of namespaces the key's Role applies in; ["*"]
// is cluster-wide. SQL NULL, the state of every key minted before namespaces
// existed, also means cluster-wide. Any other empty or undecodable value grants
// nothing.
//
// UserID, when set, makes the key user-bound (identity-and-access E4, design
// §7.3): it stores no role, namespaces or scope, and every request resolves
// the user's grants afresh. The stored row decides the kind, never the
// token's prefix. The column is nullable with no default, so every key minted
// before it existed reads NULL and stays a plain key.
type APIKey struct {
	ID            uuid.UUID      `gorm:"type:uuid;primaryKey" json:"id"`
	KeyPrefix     string         `gorm:"type:text;size:12;not null;index" json:"key_prefix"`
	KeyHash       string         `gorm:"type:text;not null;uniqueIndex" json:"-"`
	BootstrapSlot *string        `gorm:"type:text;uniqueIndex" json:"-"`
	Description   string         `gorm:"type:text" json:"description,omitempty"`
	Role          Role           `gorm:"type:text;not null" json:"role"`
	Scope         datatypes.JSON `gorm:"type:json" json:"scope,omitempty"`
	Namespaces    datatypes.JSON `gorm:"type:json" json:"namespaces,omitempty"` // NULL means ["*"]; see below
	CreatedBy     string         `gorm:"type:text" json:"created_by,omitempty"`
	CreatedAt     time.Time      `gorm:"not null" json:"created_at"`
	ExpiresAt     *time.Time     `json:"expires_at,omitempty"`
	LastUsedAt    *time.Time     `json:"last_used_at,omitempty"`
	RevokedAt     *time.Time     `json:"revoked_at,omitempty"`
	UserID        *uuid.UUID     `gorm:"type:uuid;index" json:"user_id,omitempty"`
}

// IsUserBound reports whether the stored key belongs to a user (E4).
func (k *APIKey) IsUserBound() bool {
	return k != nil && k.UserID != nil
}

// IsRevoked returns true when the key has been revoked.
func (k *APIKey) IsRevoked() bool {
	return k.RevokedAt != nil
}

// IsExpired returns true when the key has passed its expiration time.
func (k *APIKey) IsExpired() bool {
	return k.ExpiresAt != nil && time.Now().UTC().After(*k.ExpiresAt)
}

// KeyScope represents optional resource scoping for an API key.
//
// A key carries at most one kind of restriction. Jobs restricts a normal
// principal to a set of job aliases (checked by the deny-by-default route-scope
// switch). Agent, when present, marks the key as a short-lived agent-session
// credential bound to exactly one incident's /v1/agent/* tool surface; an agent
// key is valid for nothing else, regardless of its Jobs field.
type KeyScope struct {
	Jobs  []string    `json:"jobs,omitempty"`
	Agent *AgentClaim `json:"agent,omitempty"`
}

// AgentClaim binds an API key to a single incident's agent tool surface. It is
// minted by the incident manager (an unscoped, server-side principal) for one
// agent session and expires with that session. The read scope is FROZEN at
// incident open: Jobs is the static job allowlist the incident manager
// snapshotted from the lineage-impact graph (excluding edges derived from the
// failing run's own outputs). The agent cannot widen it — server-side
// enforcement is the boundary, not the prompt.
type AgentClaim struct {
	// IncidentID is the only incident whose /v1/agent/* routes this key may call.
	IncidentID uuid.UUID `json:"incident_id"`
	// Jobs is the frozen job allowlist governing which jobs' read-only context
	// (logs, why, run history) the agent may pull through the context routes.
	Jobs []string `json:"jobs,omitempty"`
	// NamespaceConfined marks a credential minted for a session confined to
	// the incident's namespace (identity-and-access A8). A credential minted
	// before that could read other namespaces' jobs, so what it writes is
	// withheld from namespace-confined readers.
	NamespaceConfined bool `json:"namespace_confined,omitempty"`
}
