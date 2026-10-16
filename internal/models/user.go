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

	// Group-refresh state (identity-and-access §11.1). Every column is
	// nullable with no default, so rows from before it existed read as zero
	// values (no grant, version 0, never refreshed, no lease, certain), and
	// none of it ever serialises: the grant is a credential.
	//
	// RefreshGrant is the provider's refresh handle sealed by
	// internal/auth/refresh_crypto.go under a key derived from
	// CAESIUM_AUTH_KEY_HASH_SECRET and bound to this user's ID. NULL means
	// no grant (SAML, LDAP, an IdP that issued none, or a grant cleared
	// after a definitive denial).
	RefreshGrant []byte `json:"-"`
	// RefreshGrantVersion advances on every grant change (login, successful
	// refresh, clear), always by a database increment. Lease claims and
	// refresh outcomes are fenced on it, so an outcome for a replaced grant
	// is discarded.
	RefreshGrantVersion int64 `json:"-"`
	// GroupsRefreshedAt is when Groups was last confirmed by the IdP: at
	// login or by a successful refresh. NULL for users from before E1.
	GroupsRefreshedAt *time.Time `json:"-"`
	// RefreshLeaseOwner and RefreshLeaseUntil are the short lease a refresher
	// claims by compare-and-swap before contacting the IdP.
	RefreshLeaseOwner *string    `json:"-"`
	RefreshLeaseUntil *time.Time `json:"-"`
	// RefreshUncertain marks a grant that may already have been redeemed
	// without a recorded outcome; a later invalid_grant then clears the grant
	// instead of revoking the user's credentials.
	RefreshUncertain bool `json:"-"`
}

// IsDisabled reports whether the user account has been disabled.
func (u *User) IsDisabled() bool {
	return u.DisabledAt != nil
}
