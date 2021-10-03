package models

import (
	"time"

	"github.com/caesium-cloud/caesium/db"
	"github.com/caesium-cloud/caesium/pkg/compare"
	"github.com/google/uuid"
)

var (
	TriggerColumns = []string{}
	TriggerTable   = "triggers"
	TriggerCreate  = `CREATE TABLE IF NOT EXISTS triggers (
		id				TEXT	PRIMARY KEY,
		type 			TEXT,
		configuration	TEXT,
		created_at		TIMESTAMP,
		updated_at		TIMESTAMP)`
)

type TriggerType string

const (
	TriggerTypeCron TriggerType = "cron"
)

type Trigger struct {
	ID            string      `db:"id"`
	Type          TriggerType `db:"type"`
	Configuration string      `db:"configuration"`
	CreatedAt     time.Time   `db:"created_at"`
	UpdatedAt     time.Time   `db:"updated_at"`
}

func NewTrigger(columns []string, values []interface{}) (*Trigger, error) {
	if err := compare.StringSlice(columns, TriggerColumns); err != nil {
		return nil, err
	}

	id, err := uuid.Parse(values[0].(string))
	if err != nil {
		return nil, err
	}

	return &Trigger{
		ID:            id.String(),
		Type:          TriggerType(values[1].(string)),
		Configuration: values[2].(string),
		CreatedAt:     values[3].(time.Time),
		UpdatedAt:     values[4].(time.Time),
	}, nil
}

type Triggers []*Trigger

func NewTriggers(rows *db.Rows) (triggers Triggers, err error) {
	triggers = make(Triggers, len(rows.Values))

	for i := range triggers {
		if triggers[i], err = NewTrigger(rows.Columns, rows.Values[i]); err != nil {
			return nil, err
		}
	}

	return
}
