package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// User is a human identity provisioned just-in-time from an external IdP.
type User struct {
	ID          uuid.UUID      `gorm:"type:uuid;primaryKey" json:"id"`
	Issuer      string         `gorm:"type:text;not null;uniqueIndex:idx_users_identity" json:"issuer"`
	Subject     string         `gorm:"type:text;not null;uniqueIndex:idx_users_identity" json:"subject"`
	Email       string         `gorm:"type:text;index" json:"email"`
	DisplayName string         `gorm:"type:text" json:"display_name,omitempty"`
	Groups      datatypes.JSON `gorm:"type:json" json:"groups,omitempty"`
	Role        Role           `gorm:"type:text;not null" json:"role"`
	// EmailVerified records whether the provider verified Email at the last
	// login. It is identity trust state, not a grant: only a verified email may
	// match policy users: bindings. The column is nullable with no default, so
	// rows from before it existed read NULL, which scans as false (untrusted).
	EmailVerified bool       `json:"email_verified"`
	CreatedAt     time.Time  `gorm:"not null" json:"created_at"`
	LastLoginAt   *time.Time `json:"last_login_at,omitempty"`
	DisabledAt    *time.Time `json:"disabled_at,omitempty"`
}

// IsDisabled reports whether the user account has been disabled.
func (u *User) IsDisabled() bool {
	return u.DisabledAt != nil
}
