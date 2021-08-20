package models

import (
	"time"

	"github.com/caesium-cloud/caesium/db"
	"github.com/caesium-cloud/caesium/pkg/compare"
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
		updated_at 	TIMESTAMP)`
)

type Atom struct {
	ID        uuid.UUID `db:"id"`
	Engine    string    `db:"engine"`
	Image     string    `db:"image"`
	Command   string    `db:"command"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

func NewAtom(columns []string, values []interface{}) (*Atom, error) {
	if err := compare.StringSlice(columns, AtomColumns); err != nil {
		return nil, err
	}

	return &Atom{
		ID:        uuid.MustParse(values[0].(string)),
		Engine:    values[1].(string),
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
