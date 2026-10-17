package event

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/authz"
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
	q := s.applyEventFilters(s.db.WithContext(s.ctx).Model(&models.IngestedEvent{}), req)
	if authz.Restricted(s.ctx) {
		// An ingested event is shown to a namespace-confined caller only when
		// every delivery it caused is one the caller owns (deliveryOwnedSQL):
		// at least one match, and no match that is not owned (B4).
		owned, args := s.deliveryOwnedSQL("m")
		provenance, provenanceArgs := s.provenanceOwnedSQL("ingested_events")
		q = q.Where("EXISTS (SELECT 1 FROM event_trigger_matches m WHERE m.event_id = ingested_events.id)").
			Where("NOT EXISTS (SELECT 1 FROM event_trigger_matches m WHERE m.event_id = ingested_events.id AND NOT ("+owned+"))", args...).
			Where(provenance, provenanceArgs...)
	}
	q = q.
		Order("created_at desc").
		Order("id desc").
		Limit(int(req.Limit))
	if req.Offset > 0 {
		q = q.Offset(int(req.Offset))
	}

	if err := q.Find(&events).Error; err != nil {
		return events, err
	}
	if authz.Restricted(s.ctx) {
		for i := range events {
			events[i].Data = redactLifecycleData(events[i].Source, events[i].Data)
		}
	}
	return events, nil
}

func (s *service) ListTriggerEvents(triggerID uuid.UUID, req *ListRequest) ([]TriggerEvent, error) {
	req = normalizeListRequest(req)

	rows := make([]models.EventTriggerMatch, 0, req.Limit)
	q := s.db.WithContext(s.ctx).
		Model(&models.EventTriggerMatch{}).
		Joins("JOIN ingested_events ON ingested_events.id = event_trigger_matches.event_id").
		Preload("Event").
		Where("event_trigger_matches.trigger_id = ?", triggerID)
	if authz.Restricted(s.ctx) {
		// A trigger's history keeps deliveries made for jobs that have since
		// moved or left the trigger: a confined caller sees only deliveries it
		// owns, filtered before paging (B4).
		owned, args := s.deliveryOwnedSQL("event_trigger_matches")
		provenance, provenanceArgs := s.provenanceOwnedSQL("ingested_events")
		q = q.Where(owned, args...).Where(provenance, provenanceArgs...)
	}
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
		evt := triggerEventFromMatch(row)
		if authz.Restricted(s.ctx) {
			evt.Data = redactLifecycleData(evt.Source, evt.Data)
		}
		out = append(out, evt)
	}
	return out, nil
}

// deliveryOwnedSQL is the ownership predicate of one event delivery (an
// event_trigger_matches row under alias) for the namespace scope on s.ctx
// (identity-and-access B4). Ownership comes from the delivery's own evidence,
// the runs it started, never from the trigger's or the job's current
// namespace: a delivery is owned only when it started at least one run, every
// run it started is persisted in a namespace the caller may read, and it
// recorded no skip and no error. Skipped, failed, zero-run and mixed
// deliveries are not owned, so their event bodies, skip reasons and errors
// (which can name other namespaces' jobs) never reach a confined caller, and
// nothing is partially redacted.
func (s *service) deliveryOwnedSQL(alias string) (string, []any) {
	db := s.db.WithContext(s.ctx)
	readableRuns := authz.NamespaceFilter(db.Model(&models.JobRun{}).Select("CAST(job_runs.id AS TEXT)"), "job_runs.namespace")
	runs, value := runsStartedIteratorSQL(db.Name(), alias+".runs_started")
	sql := alias + ".skipped = ? AND COALESCE(" + alias + ".skip_reason, '') = '' AND COALESCE(" + alias + ".error, '') = ''" +
		" AND EXISTS (SELECT 1 FROM " + runs + " WHERE " + value + " IS NOT NULL AND " + value + " <> '')" +
		" AND NOT EXISTS (SELECT 1 FROM " + runs + " WHERE " + value + " IS NULL OR " + value + " NOT IN (?))"
	return sql, []any{false, readableRuns}
}

// LifecycleEventSource is the source the event-trigger lifecycle bridge
// (internal/trigger/event Router.lifecycleIngestedEvent) stamps on the events
// it derives from a run's own run_completed / run_failed / run_terminal
// event; their data is that run's payload, with its id under "run_id".
const LifecycleEventSource = "caesium"

// provenanceOwnedSQL is the second half of delivery ownership for the
// ingested event under alias: an event derived from a run's lifecycle carries
// that run's payload, so a confined caller may see it only when the
// originating run (data.run_id) is persisted in a namespace it may read. An
// event of that source naming no readable run is hidden. Events from other
// sources are owned through the runs they started alone (deliveryOwnedSQL).
func (s *service) provenanceOwnedSQL(alias string) (string, []any) {
	db := s.db.WithContext(s.ctx)
	readableRuns := authz.NamespaceFilter(db.Model(&models.JobRun{}).Select("CAST(job_runs.id AS TEXT)"), "job_runs.namespace")
	runID := "CASE WHEN json_valid(" + alias + ".data) THEN json_extract(" + alias + ".data, '$.run_id') END"
	if db.Name() == "postgres" {
		runID = "(" + alias + ".data->>'run_id')"
	}
	return "(COALESCE(" + alias + ".source, '') <> ? OR " + runID + " IN (?))", []any{LifecycleEventSource, readableRuns}
}

// redactLifecycleData removes the job labels a lifecycle-bridge event copies
// from its run payload (top level and per task) before a namespace-confined
// caller receives it: they are read from the job's current row when the event
// is built, the same rule the event stream applies (identity-and-access B4).
// Other sources pass through; lifecycle data that is not a JSON object is
// dropped.
func redactLifecycleData(source string, data datatypes.JSON) datatypes.JSON {
	if source != LifecycleEventSource || len(data) == 0 || !bytes.Contains(data, []byte(`"job_labels"`)) {
		return data
	}
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil
	}
	delete(payload, "job_labels")
	if tasks, ok := payload["tasks"].([]any); ok {
		for _, task := range tasks {
			if fields, ok := task.(map[string]any); ok {
				delete(fields, "job_labels")
			}
		}
	}
	redacted, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return datatypes.JSON(redacted)
}

// runsStartedIteratorSQL iterates the JSON array of run ids in column (NULL
// or anything that is not an array iterates nothing), returning the FROM item
// and the text value expression of each element.
func runsStartedIteratorSQL(dialect, column string) (from, value string) {
	if dialect == "postgres" {
		// datatypes.JSON migrates as JSONB on PostgreSQL; casting keeps the
		// expression valid for a json column too.
		return "jsonb_array_elements_text(CASE WHEN jsonb_typeof((" + column + ")::jsonb) = 'array' THEN (" + column + ")::jsonb ELSE '[]'::jsonb END) AS rs(value)", "rs.value"
	}
	return "json_each(CASE WHEN json_valid(" + column + ") THEN CASE WHEN json_type(" + column + ") = 'array' THEN " + column + " ELSE '[]' END ELSE '[]' END) AS rs", "CAST(rs.value AS TEXT)"
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
