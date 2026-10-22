package models

import (
	"time"

	"github.com/google/uuid"
)

// CLI login transaction states (identity-and-access E5, design §13.1).
const (
	// CLILoginPending: started by the CLI, not yet confirmed in a browser.
	CLILoginPending = "pending"
	// CLILoginAuthorized: a signed-in user confirmed it; a one-time code is
	// outstanding.
	CLILoginAuthorized = "authorized"
	// CLILoginExchanged: the code was redeemed and a user-bound key minted.
	CLILoginExchanged = "exchanged"
	// CLILoginFailed: the code was presented with a wrong verifier and burned.
	CLILoginFailed = "failed"
)

// CLI login transaction modes.
const (
	// CLILoginModeLoopback redirects the browser to the CLI's loopback
	// listener at http://127.0.0.1:<Port>/callback.
	CLILoginModeLoopback = "loopback"
	// CLILoginModeManual shows the code in the browser for
	// `caesium login --code`.
	CLILoginModeManual = "manual"
)

// CLILoginTransaction is one `caesium login` attempt. It stores no secret in
// plaintext: the CLI's state is kept as a keyed hash, the challenge is already
// the S256 hash of the CLI-held verifier, the one-time code is a keyed hash,
// and no key material is ever stored (the key is minted at exchange and
// returned once).
type CLILoginTransaction struct {
	ID           uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	StateHash    string    `gorm:"type:text;not null;uniqueIndex" json:"-"`
	Challenge    string    `gorm:"type:text;not null" json:"-"`
	Mode         string    `gorm:"type:text;not null" json:"mode"`
	Port         int       `gorm:"not null" json:"port"`
	HostnameHint string    `gorm:"type:text" json:"hostname_hint,omitempty"`
	SourceIP     string    `gorm:"type:text;index" json:"source_ip,omitempty"`
	Status       string    `gorm:"type:text;not null;index" json:"status"`
	CreatedAt    time.Time `gorm:"not null" json:"created_at"`
	// ExpiresAt bounds the whole transaction (start + 5m).
	ExpiresAt time.Time `gorm:"not null;index" json:"expires_at"`

	// Set by the authorising (confirming) user.
	UserID            *uuid.UUID `gorm:"type:uuid" json:"user_id,omitempty"`
	DisableGeneration int64      `gorm:"not null;default:0" json:"-"`
	AuthorizedBy      string     `gorm:"type:text" json:"authorized_by,omitempty"`
	AuthorizedAt      *time.Time `json:"authorized_at,omitempty"`
	CodeHash          *string    `gorm:"type:text;uniqueIndex" json:"-"`
	// RedeemBy is the code's single effective deadline:
	// min(ExpiresAt, AuthorizedAt + 60s).
	RedeemBy *time.Time `json:"redeem_by,omitempty"`

	// Set at exchange.
	KeyID       *uuid.UUID `gorm:"type:uuid" json:"key_id,omitempty"`
	ExchangedAt *time.Time `json:"exchanged_at,omitempty"`
}

// TableName pins the table name.
func (CLILoginTransaction) TableName() string {
	return "cli_login_transactions"
}
