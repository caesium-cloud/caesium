package models

import (
	"time"

	"github.com/google/uuid"
)

type TriggerType string

const (
	TriggerTypeCron TriggerType = "cron"
	TriggerTypeHTTP TriggerType = "http"
)

type Trigger struct {
	ID                 uuid.UUID   `gorm:"type:uuid;primaryKey" json:"id"`
	Alias              string      `gorm:"index" json:"alias"`
	Type               TriggerType `gorm:"index;not null" json:"type"`
	Configuration      string      `json:"configuration"`
	ProvenanceSourceID string      `gorm:"index" json:"provenance_source_id"`
	ProvenanceRepo     string      `json:"provenance_repo"`
	ProvenanceRef      string      `json:"provenance_ref"`
	ProvenanceCommit   string      `json:"provenance_commit"`
	ProvenancePath     string      `json:"provenance_path"`
	CreatedAt          time.Time   `gorm:"not null" json:"created_at"`
	UpdatedAt          time.Time   `gorm:"not null" json:"updated_at"`
}

type Triggers []*Trigger
