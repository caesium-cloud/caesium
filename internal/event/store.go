package event

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Store struct {
	db *gorm.DB
}

func NewStore(db *gorm.DB) *Store {
	if db == nil {
		panic("event store requires database connection")
	}
	return &Store{db: db}
}

func (s *Store) AppendTx(tx *gorm.DB, evt *Event) error {
	if tx == nil {
		return errors.New("event: append requires transaction")
	}
	if evt == nil {
		return errors.New("event: append requires event")
	}

	now := time.Now().UTC()
	if evt.Timestamp.IsZero() {
		evt.Timestamp = now
	}

	record := &models.ExecutionEvent{
		Type:      string(evt.Type),
		JobID:     uuidPtr(evt.JobID),
		RunID:     uuidPtr(evt.RunID),
		TaskID:    uuidPtr(evt.TaskID),
		Payload:   []byte(evt.Payload),
		CreatedAt: evt.Timestamp,
	}

	if err := tx.Create(record).Error; err != nil {
		return err
	}

	evt.Sequence = record.Sequence
	evt.Timestamp = record.CreatedAt
	return nil
}

func (s *Store) LatestSequence(ctx context.Context) (uint64, error) {
	var seq uint64
	err := s.db.WithContext(ctx).
		Model(&models.ExecutionEvent{}).
		Select("COALESCE(MAX(sequence), 0)").
		Scan(&seq).Error
	return seq, err
}

func (s *Store) ListSince(ctx context.Context, after uint64, limit int, filter Filter) ([]Event, error) {
	if limit <= 0 {
		limit = 500
	}

	var rows []models.ExecutionEvent
	query := s.db.WithContext(ctx).
		Model(&models.ExecutionEvent{}).
		Where("sequence > ?", after).
		Order("sequence ASC").
		Limit(limit)

	if filter.JobID != uuid.Nil {
		query = query.Where("job_id = ?", filter.JobID)
	}
	if filter.RunID != uuid.Nil {
		query = query.Where("run_id = ?", filter.RunID)
	}
	if len(filter.Types) > 0 {
		types := make([]string, 0, len(filter.Types))
		for _, typ := range filter.Types {
			types = append(types, string(typ))
		}
		query = query.Where("type IN ?", types)
	}

	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}

	events := make([]Event, 0, len(rows))
	for _, row := range rows {
		events = append(events, modelToEvent(row))
	}
	return events, nil
}

func uuidPtr(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	value := id
	return &value
}

func derefUUID(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

func modelToEvent(row models.ExecutionEvent) Event {
	payload := json.RawMessage(nil)
	if len(row.Payload) > 0 {
		payload = json.RawMessage(row.Payload)
	}
	return Event{
		Sequence:  row.Sequence,
		Type:      Type(row.Type),
		JobID:     derefUUID(row.JobID),
		RunID:     derefUUID(row.RunID),
		TaskID:    derefUUID(row.TaskID),
		Timestamp: row.CreatedAt,
		Payload:   payload,
	}
}
