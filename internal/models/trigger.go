package models

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type TriggerType string

const (
	TriggerTypeCron TriggerType = "cron"
	TriggerTypeHTTP TriggerType = "http"
)

type Trigger struct {
	ID                 uuid.UUID      `gorm:"type:uuid;primaryKey" json:"id"`
	Alias              string         `gorm:"index" json:"alias"`
	Type               TriggerType    `gorm:"index;not null" json:"type"`
	NormalizedPath     string         `gorm:"index" json:"-"`
	Configuration      string         `json:"configuration"`
	ProvenanceSourceID string         `gorm:"index" json:"provenance_source_id"`
	ProvenanceRepo     string         `json:"provenance_repo"`
	ProvenanceRef      string         `json:"provenance_ref"`
	ProvenanceCommit   string         `json:"provenance_commit"`
	ProvenancePath     string         `json:"provenance_path"`
	DeletedAt          gorm.DeletedAt `gorm:"index" json:"-"`
	CreatedAt          time.Time      `gorm:"not null" json:"created_at"`
	UpdatedAt          time.Time      `gorm:"not null" json:"updated_at"`
}

type Triggers []*Trigger

func (t *Trigger) BeforeSave(*gorm.DB) error {
	return t.ApplyDerivedFields()
}

func (t *Trigger) ApplyDerivedFields() error {
	normalized, err := NormalizedTriggerPathForConfiguration(t.Type, t.Configuration)
	if err != nil {
		return err
	}
	t.NormalizedPath = normalized
	return nil
}

func NormalizedTriggerPath(path string) string {
	trimmed := strings.Trim(strings.TrimSpace(path), "/")
	if trimmed == "" {
		return ""
	}

	parts := strings.Split(trimmed, "/")
	switch {
	case len(parts) >= 2 && parts[0] == "v1" && parts[1] == "hooks":
		parts = parts[2:]
	case parts[0] == "hooks":
		parts = parts[1:]
	}

	return strings.Trim(strings.Join(parts, "/"), "/")
}

func NormalizedTriggerPathForConfiguration(triggerType TriggerType, configuration string) (string, error) {
	if triggerType != TriggerTypeHTTP || strings.TrimSpace(configuration) == "" {
		return "", nil
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(configuration), &cfg); err != nil {
		return "", err
	}

	path, _ := cfg["path"].(string)
	return NormalizedTriggerPath(path), nil
}
