package models

import (
	"time"

	"github.com/google/uuid"
)

var (
	CallbackColumns = []string{}
	CallbackTable   = "callbacks"
	CallbackCreate  = `CREATE TABLE IF NOT EXISTS callbacks (
		id				TEXT		PRIMARY KEY,
		type			TEXT,
		configuration	TEXT,
		job_id			TEXT,
		created_at		TIMESTAMP,
		updated_at		TIMESTAMP)`
)

type CallbackType string

const (
	CallbackTypeNotification CallbackType = "notification"
)

type Callback struct {
	ID            uuid.UUID    `db:"id"`
	Type          CallbackType `db:"type"`
	Configuration string       `db:"configuration"`
	JobID         uuid.UUID    `db:"job_id"`
	CreatedAt     time.Time    `db:"created_at"`
	UpdatedAt     time.Time    `db:"updated_at"`
}
