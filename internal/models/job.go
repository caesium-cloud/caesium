package models

import (
	"fmt"
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

func (j *Job) String() string {
	return fmt.Sprintf("ID:%s\tAlias:%s\t", j.ID, j.Alias)
}

type Jobs []*Job
