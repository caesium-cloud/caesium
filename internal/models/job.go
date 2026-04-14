package models

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type Job struct {
	ID                 uuid.UUID         `gorm:"type:uuid;primaryKey" json:"id"`
	Alias              string            `gorm:"uniqueIndex" json:"alias"`
	TriggerID          uuid.UUID         `gorm:"type:uuid;index;not null" json:"trigger_id"`
	Trigger            Trigger           `gorm:"constraint:OnDelete:CASCADE" json:"-"`
	Labels             datatypes.JSONMap `gorm:"type:json" json:"labels"`
	Annotations        datatypes.JSONMap `gorm:"type:json" json:"annotations"`
	ProvenanceSourceID string            `gorm:"index" json:"provenance_source_id"`
	ProvenanceRepo     string            `json:"provenance_repo"`
	ProvenanceRef      string            `json:"provenance_ref"`
	ProvenanceCommit   string            `json:"provenance_commit"`
	ProvenancePath     string            `json:"provenance_path"`
	MaxParallelTasks   int               `json:"max_parallel_tasks"`
	TaskTimeout        time.Duration     `json:"task_timeout"`
	RunTimeout         time.Duration     `json:"run_timeout"`
	SLA                datatypes.JSON    `gorm:"type:json" json:"sla,omitempty"`
	// SchemaValidation controls runtime output schema validation for this job's tasks.
	// Values: "" (disabled), "warn" (log violations), "fail" (fail task on violation).
	SchemaValidation string         `gorm:"type:text;not null;default:''" json:"schema_validation,omitempty"`
	CacheConfig      datatypes.JSON `gorm:"type:json" json:"cache_config,omitempty"`
	Paused           bool           `gorm:"not null;default:false" json:"paused"`
	DeletedAt        gorm.DeletedAt `gorm:"index" json:"-"`
	CreatedAt        time.Time      `gorm:"not null" json:"created_at"`
	UpdatedAt        time.Time      `gorm:"not null" json:"updated_at"`

	LatestRun *JobRun `gorm:"-" json:"latest_run,omitempty"`
}

func (j *Job) String() string {
	return fmt.Sprintf("ID:%s\tAlias:%s\t", j.ID, j.Alias)
}

type Jobs []*Job
