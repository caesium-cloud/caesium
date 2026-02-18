package models

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

type Job struct {
	ID                 uuid.UUID         `gorm:"type:uuid;primaryKey" json:"id"`
	Alias              string            `gorm:"index" json:"alias"`
	TriggerID          uuid.UUID         `gorm:"type:uuid;index;not null" json:"trigger_id"`
	Labels             datatypes.JSONMap `gorm:"type:json" json:"labels"`
	Annotations        datatypes.JSONMap `gorm:"type:json" json:"annotations"`
	ProvenanceSourceID string            `gorm:"index" json:"provenance_source_id"`
	ProvenanceRepo     string            `json:"provenance_repo"`
	ProvenanceRef      string            `json:"provenance_ref"`
	ProvenanceCommit   string            `json:"provenance_commit"`
	ProvenancePath     string            `json:"provenance_path"`
	CreatedAt          time.Time         `gorm:"not null" json:"created_at"`
	UpdatedAt          time.Time         `gorm:"not null" json:"updated_at"`

	LatestRun *JobRun `gorm:"-" json:"latest_run,omitempty"`
}

func (j *Job) String() string {
	return fmt.Sprintf("ID:%s\tAlias:%s\t", j.ID, j.Alias)
}

type Jobs []*Job
