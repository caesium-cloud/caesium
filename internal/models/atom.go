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
	ID        uuid.UUID  `gorm:"type:uuid;primaryKey"`
	Engine    AtomEngine `gorm:"index;not null"`
	Image     string     `gorm:"index;not null"`
	Command   string     `gorm:"command"`
	CreatedAt time.Time  `gorm:"not null"`
	UpdatedAt time.Time  `gorm:"not null"`
}

func (a *Atom) Cmd() []string {
	cmd := []string{}
	if err := json.Unmarshal([]byte(a.Command), &cmd); err != nil {
		return nil
	}
	return cmd
}

type Atoms []*Atom
