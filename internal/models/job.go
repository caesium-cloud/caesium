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
	Priority           string            `gorm:"type:text;not null;default:''" json:"priority,omitempty"`
	Concurrency        datatypes.JSON    `gorm:"type:json" json:"concurrency,omitempty"`
	RateLimits         datatypes.JSON    `gorm:"type:json" json:"rate_limits,omitempty"`
	SLA                datatypes.JSON    `gorm:"type:json" json:"sla,omitempty"`
	// SchemaValidation controls runtime output schema validation for this job's tasks.
	// Values: "" (disabled), "warn" (log violations), "fail" (fail task on violation).
	SchemaValidation string `gorm:"type:text;not null;default:''" json:"schema_validation,omitempty"`
	// OnUpstreamHold persists metadata.onUpstreamHold ("" = the "skip" default,
	// or "run"). It is a scalar column for the same reason SchemaValidation is:
	// the run-admission gate (data-circuit-breaker C2) decides inside a store
	// transaction with no jobdef in hand, and re-parsing the stored definition on
	// the admission hot path is not an option. It also keeps `caesium job diff`
	// and the tier-3 ApprovalRequest diff honest — an enforced policy that
	// rendered as "no changes" is how an approver waves through a policy edit.
	OnUpstreamHold string         `gorm:"type:text;not null;default:''" json:"on_upstream_hold,omitempty"`
	ReplaySafe     bool           `gorm:"not null;default:false" json:"replay_safe"`
	CacheConfig    datatypes.JSON `gorm:"type:json" json:"cache_config,omitempty"`
	// Remediation persists the job's `metadata.remediation` block verbatim (see
	// pkg/jobdef.MetadataRemediation). It is what makes a job's agent policy
	// ENFORCEABLE rather than merely lintable: the action executor resolves the
	// effective playbook from this column, so a job that narrows what the agent
	// may do autonomously is evaluated under its own policy instead of the
	// deployment-wide default profile. Empty means "the job declared no policy".
	Remediation datatypes.JSON `gorm:"type:json" json:"remediation,omitempty"`
	Paused      bool           `gorm:"not null;default:false" json:"paused"`
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"-"`
	CreatedAt   time.Time      `gorm:"not null" json:"created_at"`
	UpdatedAt   time.Time      `gorm:"not null" json:"updated_at"`

	LatestRun *JobRun `gorm:"-" json:"latest_run,omitempty"`
}

func (j *Job) String() string {
	return fmt.Sprintf("ID:%s\tAlias:%s\t", j.ID, j.Alias)
}

type Jobs []*Job
