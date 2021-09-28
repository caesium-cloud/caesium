package models

import (
	"time"

	"github.com/caesium-cloud/caesium/db"
	"github.com/caesium-cloud/caesium/pkg/compare"
	"github.com/google/uuid"
)

var (
	JobColumns = []string{}
	JobTable   = "jobs"
	JobCreate  = `CREATE TABLE IF NOT EXISTS jobs (
		id			TEXT,
		alias		TEXT,
		trigger_id	TEXT,
		created_at	TIMESTAMP,
		updated_at	TIMESTAMP,
		PRIMARY KEY (id, alias))`
)

type Job struct {
	ID        uuid.UUID `db:"id"`
	Alias     string    `db:"alias"`
	TriggerID uuid.UUID `db:"trigger_id"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

func NewJob(columns []string, values []interface{}) (*Job, error) {
	if err := compare.StringSlice(columns, JobColumns); err != nil {
		return nil, err
	}

	id, err := uuid.Parse(values[0].(string))
	if err != nil {
		return nil, err
	}

	triggerID, err := uuid.Parse(values[2].(string))
	if err != nil {
		return nil, err
	}

	return &Job{
		ID:        id,
		Alias:     values[1].(string),
		TriggerID: triggerID,
		CreatedAt: values[3].(time.Time),
		UpdatedAt: values[4].(time.Time),
	}, nil
}

type Jobs []*Job

func NewJobs(rows *db.Rows) (jobs Jobs, err error) {
	jobs = make(Jobs, len(rows.Values))

	for i := range jobs {
		if jobs[i], err = NewJob(rows.Columns, rows.Values[i]); err != nil {
			return nil, err
		}
	}

	return
}
