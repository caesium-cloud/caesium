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

const defaultIngestRetentionPruneInterval = time.Hour
const minIngestRetentionPruneInterval = time.Minute

type IngestStore struct {
	db  *gorm.DB
	now func() time.Time
}

func NewIngestStore(db *gorm.DB) *IngestStore {
	if db == nil {
		panic("event ingest store requires database connection")
	}
	return &IngestStore{
		db:  db,
		now: func() time.Time { return time.Now().UTC() },
	}
}

func (s *IngestStore) Create(ctx context.Context, evt *models.IngestedEvent) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return s.CreateTx(tx, evt)
	})
}

func (s *IngestStore) CreateTx(tx *gorm.DB, evt *models.IngestedEvent) error {
	if tx == nil {
		return errors.New("event ingest: create requires transaction")
	}
	if evt == nil {
		return errors.New("event ingest: create requires event")
	}

	evt.Type = strings.TrimSpace(evt.Type)
	evt.Source = strings.TrimSpace(evt.Source)
	if evt.Type == "" {
		return errors.New("event ingest: type is required")
	}
	if evt.ID == uuid.Nil {
		evt.ID = uuid.New()
	}
	if evt.CreatedAt.IsZero() {
		evt.CreatedAt = s.now()
	}
	if len(evt.Data) == 0 {
		evt.Data = []byte("{}")
	}
	if !json.Valid(evt.Data) {
		return fmt.Errorf("event ingest: data must be valid JSON")
	}

	return tx.Create(evt).Error
}

func (s *IngestStore) RecordMatchesTx(tx *gorm.DB, matches []*models.EventTriggerMatch) error {
	if tx == nil {
		return errors.New("event ingest: record matches requires transaction")
	}
	if len(matches) == 0 {
		return nil
	}

	now := s.now()
	for _, match := range matches {
		if match == nil {
			return errors.New("event ingest: record matches requires non-nil rows")
		}
		if match.ID == uuid.Nil {
			match.ID = uuid.New()
		}
		if match.EventID == uuid.Nil {
			return errors.New("event ingest: match event_id is required")
		}
		if match.TriggerID == uuid.Nil {
			return errors.New("event ingest: match trigger_id is required")
		}
		if match.MatchedAt.IsZero() {
			match.MatchedAt = now
		}
	}

	return tx.Create(&matches).Error
}

func (s *IngestStore) Prune(ctx context.Context, retention time.Duration) (int, error) {
	if retention <= 0 {
		return 0, nil
	}
	cutoff := s.now().Add(-retention)

	var total int64
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		matchResult := tx.Where("matched_at <= ?", cutoff).Delete(&models.EventTriggerMatch{})
		if matchResult.Error != nil {
			return matchResult.Error
		}
		total += matchResult.RowsAffected

		eventResult := tx.Where("created_at <= ?", cutoff).Delete(&models.IngestedEvent{})
		if eventResult.Error != nil {
			return eventResult.Error
		}
		total += eventResult.RowsAffected
		return nil
	})
	return int(total), err
}

func StartIngestRetentionPruner(ctx context.Context, store *IngestStore, retention time.Duration) {
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
					log.Error("event retention pruner failed", "error", err)
					continue
				}
				if count > 0 {
					log.Info("event retention pruner removed old rows", "count", count)
				}
			}
		}
	}()
}
