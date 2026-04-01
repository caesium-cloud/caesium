package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type CallbackType string

const (
	CallbackTypeNotification CallbackType = "notification"
)

type Callback struct {
	ID            uuid.UUID      `gorm:"type:uuid;primaryKey"`
	Type          CallbackType   `gorm:"index;type:string;not null"`
	Configuration string         `gorm:"not null"`
	JobID         uuid.UUID      `gorm:"index;not null"`
	Job           Job            `gorm:"constraint:OnDelete:CASCADE" json:"-"`
	Position      int            `gorm:"not null;default:0" json:"-"`
	DeletedAt     gorm.DeletedAt `gorm:"index" json:"-"`
	CreatedAt     time.Time      `gorm:"not null"`
	UpdatedAt     time.Time      `gorm:"not null"`
}

type Callbacks []*Callback
