package models

import (
	"time"

	"github.com/google/uuid"
)

type JobRun struct {
	ID          uuid.UUID `gorm:"type:uuid;primaryKey"`
	JobID       uuid.UUID `gorm:"type:uuid;index;not null"`
	Status      string    `gorm:"type:text;index;not null"`
	Error       string
	StartedAt   time.Time `gorm:"not null"`
	CompletedAt *time.Time
	CreatedAt   time.Time  `gorm:"not null"`
	UpdatedAt   time.Time  `gorm:"not null"`
	Tasks       []*TaskRun `gorm:"foreignKey:JobRunID;constraint:OnDelete:CASCADE"`
}

type TaskRun struct {
	ID                      uuid.UUID  `gorm:"type:uuid;primaryKey"`
	JobRunID                uuid.UUID  `gorm:"type:uuid;index;not null"`
	TaskID                  uuid.UUID  `gorm:"type:uuid;index;not null"`
	AtomID                  uuid.UUID  `gorm:"type:uuid;index;not null"`
	Engine                  AtomEngine `gorm:"type:text;not null"`
	Image                   string     `gorm:"not null"`
	Command                 string     `gorm:"not null"`
	Status                  string     `gorm:"type:text;index;not null"`
	Result                  string
	Error                   string
	RuntimeID               string
	OutstandingPredecessors int `gorm:"not null"`
	StartedAt               *time.Time
	CompletedAt             *time.Time
	CreatedAt               time.Time `gorm:"not null"`
	UpdatedAt               time.Time `gorm:"not null"`
}
