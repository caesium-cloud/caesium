package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// ChannelType identifies the notification transport.
type ChannelType string

const (
	ChannelTypeWebhook   ChannelType = "webhook"
	ChannelTypeSlack     ChannelType = "slack"
	ChannelTypeEmail     ChannelType = "email"
	ChannelTypePagerDuty ChannelType = "pagerduty"
	ChannelTypeAIAgent   ChannelType = "ai_agent"
)

// NotificationChannel stores a configured notification destination.
type NotificationChannel struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	// Namespace is the tenancy namespace; "*" (ClusterNamespace) marks a
	// cluster-shared channel. It is create-only.
	Namespace string         `gorm:"type:text;not null;default:'default';index" json:"namespace"`
	Name      string         `gorm:"uniqueIndex;not null" json:"name"`
	Type      ChannelType    `gorm:"type:text;not null;index" json:"type"`
	Config    datatypes.JSON `gorm:"type:json;not null" json:"config"`
	Enabled   bool           `gorm:"not null;default:true" json:"enabled"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
	CreatedAt time.Time      `gorm:"not null" json:"created_at"`
	UpdatedAt time.Time      `gorm:"not null" json:"updated_at"`
}

// NotificationPolicy links event types to channels with optional filters.
type NotificationPolicy struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	// Namespace is the tenancy namespace; "*" (ClusterNamespace) marks a
	// cluster-shared policy. It is create-only, and the policy may reference only
	// a channel in its own namespace or "*".
	Namespace  string              `gorm:"type:text;not null;default:'default';index" json:"namespace"`
	Name       string              `gorm:"uniqueIndex;not null" json:"name"`
	ChannelID  uuid.UUID           `gorm:"type:uuid;index;not null" json:"channel_id"`
	Channel    NotificationChannel `gorm:"constraint:OnDelete:CASCADE" json:"-"`
	EventTypes datatypes.JSON      `gorm:"type:json;not null" json:"event_types"`
	Filters    datatypes.JSON      `gorm:"type:json" json:"filters,omitempty"`
	Enabled    bool                `gorm:"not null;default:true" json:"enabled"`
	DeletedAt  gorm.DeletedAt      `gorm:"index" json:"-"`
	CreatedAt  time.Time           `gorm:"not null" json:"created_at"`
	UpdatedAt  time.Time           `gorm:"not null" json:"updated_at"`
}
