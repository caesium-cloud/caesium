package models

import (
	"time"

	"github.com/google/uuid"
)

// ContractAck records an intentional, time-bounded acknowledgement of a
// breaking contract edge set. It is a low-volume catalog table; run hot paths
// must not depend on it.
type ContractAck struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`

	// Dataset is the human-readable contract subject: an OpenLineage dataset
	// identity when present, otherwise the producer output-key subject for an
	// inferred event-trigger edge.
	Dataset string `gorm:"type:text;not null;index:idx_contract_ack_dataset_digest,priority:1" json:"dataset"`
	// EdgeSetDigest is a deterministic digest over the breaking edge set the
	// ack covers. C2's --allow-breaking path writes this exact value.
	EdgeSetDigest string `gorm:"type:text;not null;index:idx_contract_ack_dataset_digest,priority:2;index" json:"edge_set_digest"`

	Actor  string `gorm:"type:text;not null;default:''" json:"actor"`
	Reason string `gorm:"type:text;not null;default:''" json:"reason"`

	CreatedAt time.Time `gorm:"not null" json:"created_at"`
	ExpiresAt time.Time `gorm:"not null;index" json:"expires_at"`
}
