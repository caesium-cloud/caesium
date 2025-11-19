package models

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type AtomEngine string

const (
	AtomEngineDocker     AtomEngine = "docker"
	AtomEngineKubernetes AtomEngine = "kubernetes"
	AtomEnginePodman     AtomEngine = "podman"
)

type Atom struct {
	ID                 uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	Engine             AtomEngine `gorm:"index;not null" json:"engine"`
	Image              string     `gorm:"index;not null" json:"image"`
	Command            string     `gorm:"command" json:"command"`
	ProvenanceSourceID string     `gorm:"index" json:"provenance_source_id"`
	ProvenanceRepo     string     `json:"provenance_repo"`
	ProvenanceRef      string     `json:"provenance_ref"`
	ProvenanceCommit   string     `json:"provenance_commit"`
	ProvenancePath     string     `json:"provenance_path"`
	CreatedAt          time.Time  `gorm:"not null" json:"created_at"`
	UpdatedAt          time.Time  `gorm:"not null" json:"updated_at"`
}

func (a *Atom) Cmd() []string {
	cmd := []string{}
	if err := json.Unmarshal([]byte(a.Command), &cmd); err != nil {
		return nil
	}
	return cmd
}

type Atoms []*Atom
