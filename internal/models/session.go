package models

import (
	"time"

	"github.com/google/uuid"
)

// Session is a server-side login session. The opaque token is never stored;
// only its keyed hash (TokenHash) is persisted.
type Session struct {
	ID                uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	UserID            uuid.UUID  `gorm:"type:uuid;not null;index" json:"user_id"`
	TokenHash         string     `gorm:"type:text;not null;uniqueIndex" json:"-"`
	CSRFToken         string     `gorm:"type:text;not null" json:"-"`
	AuthMethod        string     `gorm:"type:text" json:"auth_method"`
	CreatedAt         time.Time  `gorm:"not null" json:"created_at"`
	IdleExpiresAt     time.Time  `gorm:"not null" json:"idle_expires_at"`
	AbsoluteExpiresAt time.Time  `gorm:"not null" json:"absolute_expires_at"`
	LastSeenAt        *time.Time `json:"last_seen_at,omitempty"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
	SourceIP          string     `gorm:"type:text" json:"source_ip,omitempty"`
	UserAgent         string     `gorm:"type:text" json:"user_agent,omitempty"`
}

// IsRevoked reports whether the session was explicitly revoked.
func (s *Session) IsRevoked() bool {
	return s.RevokedAt != nil
}
