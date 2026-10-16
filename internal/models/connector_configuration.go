package models

import (
	"time"

	"gorm.io/datatypes"
)

// ConnectorConfigurationID is the only row in connector_configurations.
// The active fingerprint lives on that singleton. Activation is a
// compare-and-swap on Fingerprint, not a second row.
const ConnectorConfigurationID = "active"

// ConnectorConfiguration is the active connector configuration epoch.
// One catalog row. Not a hot execution table, and not registered in
// hotPathModels()/hotTables. Identity history remains when a connection is removed so a later file
// cannot reuse its id for another provider target.
type ConnectorConfiguration struct {
	ID          string         `gorm:"type:text;primaryKey" json:"id"`
	Identities  datatypes.JSON `gorm:"type:json" json:"-"`
	Fingerprint string         `gorm:"type:text;not null" json:"fingerprint"`
	UpdatedAt   time.Time      `gorm:"not null" json:"updated_at"`
}

// TableName is the catalog epoch table.
func (ConnectorConfiguration) TableName() string { return "connector_configurations" }
