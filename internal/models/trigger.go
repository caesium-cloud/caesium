package models

import (
	"time"

	"github.com/google/uuid"
)

type TriggerType string

const (
	TriggerTypeCron TriggerType = "cron"
	TriggerTypeHTTP TriggerType = "http"
)

type Trigger struct {
	ID            uuid.UUID   `gorm:"type:uuid;primaryKey"`
	Alias         string      `gorm:"index"`
	Type          TriggerType `gorm:"index;not null"`
	Configuration string
	CreatedAt     time.Time `gorm:"not null"`
	UpdatedAt     time.Time `gorm:"not null"`
}

type Triggers []*Trigger
