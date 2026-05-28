package models

import "time"

// SAMLAssertionReplay records accepted SAML assertion IDs so a signed
// assertion cannot be replayed against another node before it expires.
type SAMLAssertionReplay struct {
	Issuer      string    `gorm:"type:text;not null;uniqueIndex:idx_saml_assertion_ids_identity" json:"issuer"`
	AssertionID string    `gorm:"type:text;not null;uniqueIndex:idx_saml_assertion_ids_identity" json:"assertion_id"`
	ExpiresAt   time.Time `gorm:"not null;index" json:"expires_at"`
	CreatedAt   time.Time `gorm:"not null" json:"created_at"`
}

func (SAMLAssertionReplay) TableName() string {
	return "saml_assertion_ids"
}
