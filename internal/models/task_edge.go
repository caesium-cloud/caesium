package models

import (
	"time"

	"github.com/google/uuid"
)

type TaskEdge struct {
	ID                 uuid.UUID `gorm:"type:uuid;primaryKey"`
	JobID              uuid.UUID `gorm:"type:uuid;index;not null"`
	FromTaskID         uuid.UUID `gorm:"type:uuid;index;not null"`
	ToTaskID           uuid.UUID `gorm:"type:uuid;index;not null"`
	ProvenanceSourceID string    `gorm:"index"`
	ProvenanceRepo     string
	ProvenanceRef      string
	ProvenanceCommit   string
	ProvenancePath     string
	CreatedAt          time.Time `gorm:"not null"`
	UpdatedAt          time.Time `gorm:"not null"`
}

type TaskEdges []*TaskEdge
