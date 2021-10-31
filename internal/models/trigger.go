package models

import (
	"time"
)

type TriggerType string

const (
	TriggerTypeCron TriggerType = "cron"
)

type Trigger struct {
	ID            string      `gorm:"type:uuid;primaryKey"`
	Type          TriggerType `gorm:"index;not null"`
	Configuration string
	CreatedAt     time.Time `gorm:"not null"`
	UpdatedAt     time.Time `gorm:"not null"`
}

type Triggers []*Trigger
