package models

import (
	"time"

	"github.com/caesium-cloud/caesium/db"
	"github.com/caesium-cloud/caesium/pkg/compare"
	"github.com/google/uuid"
)

var (
	TaskColumns = []string{}
	TaskTable   = "tasks"
	TaskCreate  = `CREATE TABLE IF NOT EXISTS tasks (
		id			TEXT	PRIMARY KEY,
		job_id		TEXT	NOT NULL,
		atom_id		TEXT	NOT NULL,
		next_id		TEXT,
		created_at	TIMESTAMP,
		updated_at	TIMESTAMP)`
)

type Task struct {
	ID        uuid.UUID  `db:"id"`
	JobID     uuid.UUID  `db:"job_id"`
	AtomID    uuid.UUID  `db:"atom_id"`
	NextID    *uuid.UUID `db:"next_id"`
	CreatedAt time.Time  `db:"created_at"`
	UpdatedAt time.Time  `db:"updated_at"`
}

func NewTask(columns []string, values []interface{}) (*Task, error) {
	if err := compare.StringSlice(columns, TaskColumns); err != nil {
		return nil, err
	}

	id, err := uuid.Parse(values[0].(string))
	if err != nil {
		return nil, err
	}

	jobID, err := uuid.Parse(values[1].(string))
	if err != nil {
		return nil, err
	}

	atomID, err := uuid.Parse(values[2].(string))
	if err != nil {
		return nil, err
	}

	t := &Task{
		ID:        id,
		JobID:     jobID,
		AtomID:    atomID,
		CreatedAt: values[4].(time.Time),
		UpdatedAt: values[5].(time.Time),
	}

	if values[3] != nil {
		nextID, err := uuid.Parse(values[3].(string))
		if err != nil {
			return nil, err
		}

		t.NextID = &nextID
	}

	return t, nil
}

type Tasks []*Task

func NewTasks(rows *db.Rows) (jobs Tasks, err error) {
	jobs = make(Tasks, len(rows.Values))

	for i := range jobs {
		if jobs[i], err = NewTask(rows.Columns, rows.Values[i]); err != nil {
			return nil, err
		}
	}

	return
}
