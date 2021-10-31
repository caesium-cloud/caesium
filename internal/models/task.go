package models

import (
	"time"

	"github.com/google/uuid"
)

type Task struct {
	ID        uuid.UUID  `gorm:"type:uuid;primaryKey"`
	JobID     uuid.UUID  `gorm:"type:uuid;index;not null"`
	AtomID    uuid.UUID  `gorm:"type:uuid;index;not null"`
	NextID    *uuid.UUID `gorm:"type:uuid;index"`
	CreatedAt time.Time  `gorm:"not null"`
	UpdatedAt time.Time  `gorm:"not null"`
}

type Tasks []*Task
