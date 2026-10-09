package models

import (
	"strconv"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// ConnectorOperation is one action receipt. The (connection, idempotency key)
// unique index collapses retries onto the original row. OpenKey is set while
// the operation is open and NULL once it is completed or rejected.
//
// dqlite/SQLite unique indexes ignore NULLs, so terminal rows do not collide
// and a later admission can insert a new row. This is the same nullable
// unique-key shape as DatasetHold.ActiveKey. A select-then-insert is not the
// guard; the unique indexes are.
//
// The row stores a request fingerprint and a bounded actor record, not secret
// bytes and not a raw action payload. It is catalog metadata, absent from
// hotPathModels()/hotTables, and it has no foreign key to job_runs or task_runs.
type ConnectorOperation struct {
	ID                 uuid.UUID      `gorm:"type:uuid;primaryKey" json:"id"`
	ConnectionID       string         `gorm:"type:text;not null;uniqueIndex:idx_connector_operations_idempotency,priority:1" json:"connection_id"`
	IdempotencyKey     string         `gorm:"type:text;not null;uniqueIndex:idx_connector_operations_idempotency,priority:2" json:"idempotency_key"`
	OpaqueID           string         `gorm:"type:text;not null;index" json:"opaque_id"`
	ActionName         string         `gorm:"type:text;not null" json:"action_name"`
	RequestFingerprint string         `gorm:"type:text;not null" json:"request_fingerprint"`
	Actor              datatypes.JSON `gorm:"type:json" json:"actor,omitempty"`
	BindingVersion     string         `gorm:"type:text;not null" json:"binding_version"`
	ExternalUpdateID   string         `gorm:"type:text;not null" json:"external_update_id"`
	State              string         `gorm:"type:text;not null;index" json:"state"`
	// OpenKey guards one open operation per connection, execution, and action.
	// NULL when State is completed or rejected.
	OpenKey   *string   `gorm:"type:text;uniqueIndex:idx_connector_operations_open_key" json:"-"`
	CreatedAt time.Time `gorm:"not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"not null" json:"updated_at"`
}

// TableName is the catalog receipt table.
func (ConnectorOperation) TableName() string { return "connector_operations" }

// OperationOpenKey encodes the open-operation guard. Each part is
// length-prefixed so a slash or other delimiter inside an id cannot make
// two different triples collide. Callers clear the column by setting it NULL;
// they do not parse this value back.
func OperationOpenKey(connectionID, opaqueID, action string) string {
	return lengthPrefixed(connectionID) + lengthPrefixed(opaqueID) + lengthPrefixed(action)
}

func lengthPrefixed(value string) string {
	return strconv.Itoa(len(value)) + ":" + value + "\n"
}
