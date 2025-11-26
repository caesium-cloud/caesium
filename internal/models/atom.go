package models

import (
	"encoding/json"
	"time"

	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/google/uuid"
	"gorm.io/datatypes"
)

type AtomEngine string

const (
	AtomEngineDocker     AtomEngine = "docker"
	AtomEngineKubernetes AtomEngine = "kubernetes"
	AtomEnginePodman     AtomEngine = "podman"
)

type Atom struct {
	ID                 uuid.UUID      `gorm:"type:uuid;primaryKey" json:"id"`
	Engine             AtomEngine     `gorm:"index;not null" json:"engine"`
	Image              string         `gorm:"index;not null" json:"image"`
	Command            string         `gorm:"command" json:"command"`
	Spec               datatypes.JSON `gorm:"type:jsonb" json:"spec"`
	ProvenanceSourceID string         `gorm:"index" json:"provenance_source_id"`
	ProvenanceRepo     string         `json:"provenance_repo"`
	ProvenanceRef      string         `json:"provenance_ref"`
	ProvenanceCommit   string         `json:"provenance_commit"`
	ProvenancePath     string         `json:"provenance_path"`
	CreatedAt          time.Time      `gorm:"not null" json:"created_at"`
	UpdatedAt          time.Time      `gorm:"not null" json:"updated_at"`
}

func (a *Atom) Cmd() []string {
	cmd := []string{}
	if err := json.Unmarshal([]byte(a.Command), &cmd); err != nil {
		return nil
	}
	return cmd
}

func (a *Atom) ContainerSpec() container.Spec {
	if len(a.Spec) == 0 {
		return container.Spec{}
	}
	var spec container.Spec
	if err := json.Unmarshal(a.Spec, &spec); err != nil {
		return container.Spec{}
	}
	return spec
}

type Atoms []*Atom
