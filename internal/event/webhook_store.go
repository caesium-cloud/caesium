package event

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const defaultWebhookEventStatus = "accepted"

type WebhookEventStore struct {
	db  *gorm.DB
	now func() time.Time
}

func NewWebhookEventStore(db *gorm.DB) *WebhookEventStore {
	if db == nil {
		panic("webhook event store requires database connection")
	}
	return &WebhookEventStore{
		db:  db,
		now: func() time.Time { return time.Now().UTC() },
	}
}

func (s *WebhookEventStore) Create(ctx context.Context, evt *models.WebhookEvent) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return s.CreateTx(tx, evt)
	})
}

func (s *WebhookEventStore) CreateTx(tx *gorm.DB, evt *models.WebhookEvent) error {
	if tx == nil {
		return errors.New("webhook event: create requires transaction")
	}
	if evt == nil {
		return errors.New("webhook event: create requires event")
	}

	evt.Path = strings.TrimSpace(evt.Path)
	evt.Source = strings.TrimSpace(evt.Source)
	evt.Status = strings.TrimSpace(evt.Status)
	if evt.Path == "" {
		return errors.New("webhook event: path is required")
	}
	if evt.Status == "" {
		evt.Status = defaultWebhookEventStatus
	}
	if evt.ID == uuid.Nil {
		evt.ID = uuid.New()
	}
	if evt.ReceivedAt.IsZero() {
		evt.ReceivedAt = s.now()
	}
	if err := validateJSONField("http_trigger_ids", evt.HTTPTriggerIDs); err != nil {
		return err
	}
	if err := validateJSONField("http_job_ids", evt.HTTPJobIDs); err != nil {
		return err
	}
	if err := validateJSONField("auth_failures", evt.AuthFailures); err != nil {
		return err
	}

	return tx.Create(evt).Error
}

func (s *WebhookEventStore) Prune(ctx context.Context, retention time.Duration) (int, error) {
	if retention <= 0 {
		return 0, nil
	}
	cutoff := s.now().Add(-retention)

	result := s.db.WithContext(ctx).Where("received_at <= ?", cutoff).Delete(&models.WebhookEvent{})
	return int(result.RowsAffected), result.Error
}

func StartWebhookEventRetentionPruner(ctx context.Context, store *WebhookEventStore, retention time.Duration) {
	if store == nil || retention <= 0 {
		return
	}

	interval := defaultIngestRetentionPruneInterval
	if retention < interval {
		interval = retention
	}
	if interval < minIngestRetentionPruneInterval {
		interval = minIngestRetentionPruneInterval
	}
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				count, err := store.Prune(ctx, retention)
				if err != nil {
					log.Error("webhook event retention pruner failed", "error", err)
					continue
				}
				if count > 0 {
					log.Info("webhook event retention pruner removed old rows", "count", count)
				}
			}
		}
	}()
}

func validateJSONField(name string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if !json.Valid(data) {
		return fmt.Errorf("webhook event: %s must be valid JSON", name)
	}
	return nil
}
