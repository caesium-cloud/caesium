package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

type IngestedEvent struct {
	ID        uuid.UUID      `gorm:"type:uuid;primaryKey" json:"id"`
	Type      string         `gorm:"type:text;index;not null" json:"type"`
	Source    string         `gorm:"type:text;index" json:"source,omitempty"`
	Data      datatypes.JSON `gorm:"type:json" json:"data,omitempty"`
	CreatedAt time.Time      `gorm:"not null;index" json:"created_at"`
}
