package models

import "time"

// RunLease records ownership of a job run for the run-owner coordination
// mode (CAESIUM_RUN_OWNER_ENABLED=true). Only one node owns a run at a
// time; fencing is enforced via the Generation field and the
// owner_generation column on task_runs.
//
// This table lives in the catalog DB (cross-run, low-volume) so that
// any node can answer "who owns run R?" without knowing the run's hot shard.
type RunLease struct {
	// RunID identifies the job run. Stored as text per existing GORM convention.
	RunID string `gorm:"type:text;primaryKey" json:"run_id"`

	// OwnerNode is the CAESIUM_NODE_ADDRESS of the owning node.
	OwnerNode string `gorm:"type:text;not null" json:"owner_node"`

	// AcquiredAt is the wall-clock time the lease was written.
	AcquiredAt time.Time `gorm:"not null" json:"acquired_at"`

	// LeaseExpiresAt is when another node may take over via CAS UPDATE.
	LeaseExpiresAt time.Time `gorm:"not null" json:"lease_expires_at"`

	// Generation is incremented on every ownership transfer. All
	// coordination writes include AND owner_generation = <generation> so
	// stale-owner writes are rejected at the DB layer.
	Generation int64 `gorm:"not null" json:"generation"`
}
