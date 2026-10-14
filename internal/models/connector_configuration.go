package models

import "time"

// ConnectorConfigurationID is the only row in connector_configurations.
// The active fingerprint lives on that singleton. Activation is a
// compare-and-swap on Fingerprint, not a second row.
const ConnectorConfigurationID = "active"

// ConnectorConfiguration is the active connector configuration epoch.
// One catalog row. Not a hot execution table, and not registered in
// hotPathModels()/hotTables. Startup reads the row when the gate is on and
// does not compare fingerprints. Comparison is C1.
type ConnectorConfiguration struct {
	ID          string    `gorm:"type:text;primaryKey" json:"id"`
	Fingerprint string    `gorm:"type:text;not null" json:"fingerprint"`
	UpdatedAt   time.Time `gorm:"not null" json:"updated_at"`
}

// TableName is the catalog epoch table.
func (ConnectorConfiguration) TableName() string { return "connector_configurations" }
