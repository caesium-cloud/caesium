package models

import (
	"time"

	"github.com/google/uuid"
)

type TaskEdge struct {
	ID                 uuid.UUID `gorm:"type:uuid;primaryKey"`
	JobID              uuid.UUID `gorm:"type:uuid;index;not null"`
	Job                Job       `gorm:"constraint:OnDelete:CASCADE" json:"-"`
	FromTaskID         uuid.UUID `gorm:"type:uuid;index:idx_task_edge_from_to;not null"`
	FromTask           Task      `gorm:"foreignKey:FromTaskID;constraint:OnDelete:CASCADE" json:"-"`
	ToTaskID           uuid.UUID `gorm:"type:uuid;index:idx_task_edge_from_to;not null"`
	ToTask             Task      `gorm:"foreignKey:ToTaskID;constraint:OnDelete:CASCADE" json:"-"`
	ProvenanceSourceID string    `gorm:"index"`
	ProvenanceRepo     string
	ProvenanceRef      string
	ProvenanceCommit   string
	ProvenancePath     string
	CreatedAt          time.Time `gorm:"not null"`
	UpdatedAt          time.Time `gorm:"not null"`
}

type TaskEdges []*TaskEdge
