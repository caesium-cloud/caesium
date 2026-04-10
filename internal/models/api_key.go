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
type APIKey struct {
	ID            uuid.UUID      `gorm:"type:uuid;primaryKey" json:"id"`
	KeyPrefix     string         `gorm:"type:text;size:12;not null;index" json:"key_prefix"`
	KeyHash       string         `gorm:"type:text;not null;uniqueIndex" json:"-"`
	BootstrapSlot *string        `gorm:"type:text;uniqueIndex" json:"-"`
	Description   string         `gorm:"type:text" json:"description,omitempty"`
	Role          Role           `gorm:"type:text;not null" json:"role"`
	Scope         datatypes.JSON `gorm:"type:json" json:"scope,omitempty"`
	CreatedBy     string         `gorm:"type:text" json:"created_by,omitempty"`
	CreatedAt     time.Time      `gorm:"not null" json:"created_at"`
	ExpiresAt     *time.Time     `json:"expires_at,omitempty"`
	LastUsedAt    *time.Time     `json:"last_used_at,omitempty"`
	RevokedAt     *time.Time     `json:"revoked_at,omitempty"`
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
type KeyScope struct {
	Jobs []string `json:"jobs,omitempty"`
}
