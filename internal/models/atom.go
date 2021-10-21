package models

import (
	"encoding/json"
	"time"

	"github.com/caesium-cloud/caesium/db"
	"github.com/google/uuid"
)

var (
	AtomColumns = []string{}
	AtomTable   = "atoms"
	AtomCreate  = `CREATE TABLE IF NOT EXISTS atoms (
		id 			TEXT		PRIMARY KEY,
		engine 		TEXT,
		image		TEXT,
		command		TEXT,
		created_at 	TIMESTAMP,
		updated_at 	TIMESTAMP);`
)

type AtomEngine string

const (
	AtomEngineDocker     AtomEngine = "docker"
	AtomEngineKubernetes AtomEngine = "kubernetes"
)

type Atom struct {
	ID        uuid.UUID `gorm:"primaryKey"`
	Engine    AtomEngine
	Image     string
	Command   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (a *Atom) Cmd() []string {
	cmd := []string{}
	json.Unmarshal([]byte(a.Command), &cmd)
	return cmd
}

func NewAtom(columns []string, values []interface{}) (*Atom, error) {
	id, err := uuid.Parse(values[0].(string))
	if err != nil {
		return nil, err
	}

	return &Atom{
		ID:        id,
		Engine:    AtomEngine(values[1].(string)),
		Image:     values[2].(string),
		Command:   values[3].(string),
		CreatedAt: values[4].(time.Time),
		UpdatedAt: values[5].(time.Time),
	}, nil
}

type Atoms []*Atom

func NewAtoms(rows *db.Rows) (atoms Atoms, err error) {
	atoms = make(Atoms, len(rows.Values))

	for i := range atoms {
		if atoms[i], err = NewAtom(rows.Columns, rows.Values[i]); err != nil {
			return nil, err
		}
	}

	return
}
