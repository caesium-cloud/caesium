package event

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

const (
	defaultListLimit uint64 = 100
	maxListLimit     uint64 = 500
)

type Service interface {
	ListIngested(*ListRequest) ([]models.IngestedEvent, error)
	ListTriggerEvents(uuid.UUID, *ListRequest) ([]TriggerEvent, error)
}

type service struct {
	ctx context.Context
	db  *gorm.DB
}

type ListRequest struct {
	Type          string
	Source        string
	CreatedAfter  *time.Time
	CreatedBefore *time.Time
	Limit         uint64
	Offset        uint64
}

type TriggerEvent struct {
	MatchID     uuid.UUID      `json:"match_id"`
	EventID     uuid.UUID      `json:"event_id"`
	TriggerID   uuid.UUID      `json:"trigger_id"`
	Type        string         `json:"type"`
	Source      string         `json:"source,omitempty"`
	Data        datatypes.JSON `json:"data,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	MatchedAt   time.Time      `json:"matched_at"`
	RunsStarted []uuid.UUID    `json:"runs_started,omitempty"`
	Skipped     bool           `json:"skipped"`
	SkipReason  string         `json:"skip_reason,omitempty"`
	Error       string         `json:"error,omitempty"`
}

func New(ctx context.Context) Service {
	return NewWithDatabase(ctx, db.Connection())
}

func NewWithDatabase(ctx context.Context, conn *gorm.DB) Service {
	return &service{
		ctx: ctx,
		db:  conn,
	}
}

func ListRequestFromValues(values url.Values) (*ListRequest, error) {
	req := &ListRequest{
		Type:   strings.TrimSpace(values.Get("type")),
		Source: strings.TrimSpace(values.Get("source")),
	}

	if raw := strings.TrimSpace(values.Get("limit")); raw != "" {
		limit, err := parseUint(raw, "limit")
		if err != nil {
			return nil, err
		}
		req.Limit = limit
	}
	if raw := strings.TrimSpace(values.Get("offset")); raw != "" {
		offset, err := parseUint(raw, "offset")
		if err != nil {
			return nil, err
		}
		req.Offset = offset
	}

	var err error
	req.CreatedAfter, err = parseTimeValue(values, "created_after", "after", "from", "start")
	if err != nil {
		return nil, err
	}
	req.CreatedBefore, err = parseTimeValue(values, "created_before", "before", "to", "end")
	if err != nil {
		return nil, err
	}

	return req, nil
}

func (s *service) ListIngested(req *ListRequest) ([]models.IngestedEvent, error) {
	req = normalizeListRequest(req)

	events := make([]models.IngestedEvent, 0, req.Limit)
	q := s.applyEventFilters(s.db.WithContext(s.ctx).Model(&models.IngestedEvent{}), req).
		Order("created_at desc").
		Order("id desc").
		Limit(int(req.Limit))
	if req.Offset > 0 {
		q = q.Offset(int(req.Offset))
	}

	return events, q.Find(&events).Error
}

func (s *service) ListTriggerEvents(triggerID uuid.UUID, req *ListRequest) ([]TriggerEvent, error) {
	req = normalizeListRequest(req)

	rows := make([]models.EventTriggerMatch, 0, req.Limit)
	q := s.db.WithContext(s.ctx).
		Model(&models.EventTriggerMatch{}).
		Joins("JOIN ingested_events ON ingested_events.id = event_trigger_matches.event_id").
		Preload("Event").
		Where("event_trigger_matches.trigger_id = ?", triggerID)
	q = s.applyJoinedEventFilters(q, req).
		Order("event_trigger_matches.matched_at desc").
		Order("event_trigger_matches.id desc").
		Limit(int(req.Limit))
	if req.Offset > 0 {
		q = q.Offset(int(req.Offset))
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}

	out := make([]TriggerEvent, 0, len(rows))
	for _, row := range rows {
		out = append(out, triggerEventFromMatch(row))
	}
	return out, nil
}

func (s *service) applyEventFilters(q *gorm.DB, req *ListRequest) *gorm.DB {
	if req.Type != "" {
		q = q.Where("type = ?", req.Type)
	}
	if req.Source != "" {
		q = q.Where("source = ?", req.Source)
	}
	if req.CreatedAfter != nil {
		q = q.Where("created_at >= ?", *req.CreatedAfter)
	}
	if req.CreatedBefore != nil {
		q = q.Where("created_at <= ?", *req.CreatedBefore)
	}
	return q
}

func (s *service) applyJoinedEventFilters(q *gorm.DB, req *ListRequest) *gorm.DB {
	if req.Type != "" {
		q = q.Where("ingested_events.type = ?", req.Type)
	}
	if req.Source != "" {
		q = q.Where("ingested_events.source = ?", req.Source)
	}
	if req.CreatedAfter != nil {
		q = q.Where("ingested_events.created_at >= ?", *req.CreatedAfter)
	}
	if req.CreatedBefore != nil {
		q = q.Where("ingested_events.created_at <= ?", *req.CreatedBefore)
	}
	return q
}

func normalizeListRequest(req *ListRequest) *ListRequest {
	if req == nil {
		req = &ListRequest{}
	}
	normalized := *req
	normalized.Type = strings.TrimSpace(normalized.Type)
	normalized.Source = strings.TrimSpace(normalized.Source)
	if normalized.Limit == 0 {
		normalized.Limit = defaultListLimit
	}
	if normalized.Limit > maxListLimit {
		normalized.Limit = maxListLimit
	}
	return &normalized
}

func triggerEventFromMatch(row models.EventTriggerMatch) TriggerEvent {
	return TriggerEvent{
		MatchID:     row.ID,
		EventID:     row.EventID,
		TriggerID:   row.TriggerID,
		Type:        row.Event.Type,
		Source:      row.Event.Source,
		Data:        row.Event.Data,
		CreatedAt:   row.Event.CreatedAt,
		MatchedAt:   row.MatchedAt,
		RunsStarted: decodeRunIDs(row.RunsStarted),
		Skipped:     row.Skipped,
		SkipReason:  row.SkipReason,
		Error:       row.Error,
	}
}

func decodeRunIDs(data datatypes.JSON) []uuid.UUID {
	if len(data) == 0 {
		return nil
	}
	var raw []string
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	out := make([]uuid.UUID, 0, len(raw))
	for _, value := range raw {
		id, err := uuid.Parse(value)
		if err == nil && id != uuid.Nil {
			out = append(out, id)
		}
	}
	return out
}

func parseUint(raw, name string) (uint64, error) {
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return value, nil
}

func parseTimeValue(values url.Values, names ...string) (*time.Time, error) {
	for _, name := range names {
		raw := strings.TrimSpace(values.Get(name))
		if raw == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			parsed, err = time.Parse(time.RFC3339, raw)
			if err != nil {
				return nil, fmt.Errorf("%s must be RFC3339", name)
			}
		}
		utc := parsed.UTC()
		return &utc, nil
	}
	return nil, nil
}
