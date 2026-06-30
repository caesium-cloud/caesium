package models

import "time"

// RateLimitToken stores consumed units for one resource/window pair. This is a
// catalog table: resources are declared on jobs and remain low-cardinality.
type RateLimitToken struct {
	Resource  string    `gorm:"type:text;primaryKey;column:resource" json:"resource"`
	WindowKey string    `gorm:"type:text;primaryKey;column:window_key" json:"window_key"`
	Consumed  int       `gorm:"not null;default:0" json:"consumed"`
	LimitVal  int       `gorm:"column:limit_val;not null" json:"limit_val"`
	ExpiresAt time.Time `gorm:"not null;index" json:"expires_at"`
}

func (RateLimitToken) TableName() string {
	return "rate_limit_tokens"
}
