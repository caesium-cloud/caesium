package models

import (
	"time"

	"github.com/google/uuid"
)

type Job struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey"`
	Alias     string    `gorm:"index"`
	TriggerID uuid.UUID `gorm:"type:uuid;index;not null"`
	CreatedAt time.Time `gorm:"not null"`
	UpdatedAt time.Time `gorm:"not null"`
}

type Jobs []*Job
