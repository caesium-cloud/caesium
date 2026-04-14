package notification

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

// --- Test helpers ---

type recordingSender struct {
	mu       sync.Mutex
	payloads []Payload
}

func (r *recordingSender) Send(_ context.Context, p Payload) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.payloads = append(r.payloads, p)
	return nil
}

func (r *recordingSender) Payloads() []Payload {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Payload, len(r.payloads))
	copy(out, r.payloads)
	return out
}

// --- Tests ---

func TestBuildPayload(t *testing.T) {
	jobID := uuid.New()
	runID := uuid.New()
	taskID := uuid.New()
	now := time.Now().UTC()

	raw, _ := json.Marshal(map[string]interface{}{
		"job_alias": "etl-daily",
		"error":     "exit code 1",
	})

	evt := event.Event{
		Type:      event.TypeTaskFailed,
		JobID:     jobID,
		RunID:     runID,
		TaskID:    taskID,
		Timestamp: now,
		Payload:   raw,
	}

	p := buildPayload(evt)

	if p.EventType != event.TypeTaskFailed {
		t.Errorf("expected event type %q, got %q", event.TypeTaskFailed, p.EventType)
	}
	if p.JobID != jobID {
		t.Errorf("expected job ID %s, got %s", jobID, p.JobID)
	}
	if p.RunID != runID {
		t.Errorf("expected run ID %s, got %s", runID, p.RunID)
	}
	if p.TaskID != taskID {
		t.Errorf("expected task ID %s, got %s", taskID, p.TaskID)
	}
	if p.JobAlias != "etl-daily" {
		t.Errorf("expected job alias %q, got %q", "etl-daily", p.JobAlias)
	}
	if p.Error != "exit code 1" {
		t.Errorf("expected error %q, got %q", "exit code 1", p.Error)
	}
}

func TestBuildPayloadNoPayload(t *testing.T) {
	evt := event.Event{
		Type:      event.TypeRunFailed,
		JobID:     uuid.New(),
		RunID:     uuid.New(),
		Timestamp: time.Now().UTC(),
	}

	p := buildPayload(evt)
	if p.JobAlias != "" {
		t.Errorf("expected empty job alias, got %q", p.JobAlias)
	}
	if p.Error != "" {
		t.Errorf("expected empty error, got %q", p.Error)
	}
}

func TestPolicyMatchesEvent(t *testing.T) {
	tests := []struct {
		name       string
		eventTypes []string
		evtType    event.Type
		want       bool
	}{
		{"match", []string{"task_failed", "run_failed"}, event.TypeTaskFailed, true},
		{"no match", []string{"run_completed"}, event.TypeTaskFailed, false},
		{"empty types", []string{}, event.TypeRunFailed, false},
		{"single match", []string{"run_timed_out"}, event.TypeRunTimedOut, true},
		{"sla missed", []string{"sla_missed"}, event.TypeSLAMissed, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			typesJSON, _ := json.Marshal(tt.eventTypes)
			p := models.NotificationPolicy{
				EventTypes: typesJSON,
			}
			got := policyMatchesEvent(p, event.Event{Type: tt.evtType})
			if got != tt.want {
				t.Errorf("policyMatchesEvent() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPolicyFilterMatches(t *testing.T) {
	jobID := uuid.New()
	otherJobID := uuid.New()

	tests := []struct {
		name    string
		filter  *PolicyFilter
		evtJob  uuid.UUID
		payload json.RawMessage
		want    bool
	}{
		{
			name:   "no filter",
			filter: nil,
			evtJob: jobID,
			want:   true,
		},
		{
			name:   "job ID match",
			filter: &PolicyFilter{JobIDs: []uuid.UUID{jobID}},
			evtJob: jobID,
			want:   true,
		},
		{
			name:   "job ID no match",
			filter: &PolicyFilter{JobIDs: []uuid.UUID{otherJobID}},
			evtJob: jobID,
			want:   false,
		},
		{
			name:    "job alias match",
			filter:  &PolicyFilter{JobAlias: "etl"},
			evtJob:  jobID,
			payload: mustJSON(map[string]string{"job_alias": "etl"}),
			want:    true,
		},
		{
			name:    "job alias no match",
			filter:  &PolicyFilter{JobAlias: "etl"},
			evtJob:  jobID,
			payload: mustJSON(map[string]string{"job_alias": "ingest"}),
			want:    false,
		},
		{
			name:    "job alias filter with nil payload fails closed",
			filter:  &PolicyFilter{JobAlias: "etl"},
			evtJob:  jobID,
			payload: nil,
			want:    false,
		},
		{
			name:    "job alias filter with unparseable payload fails closed",
			filter:  &PolicyFilter{JobAlias: "etl"},
			evtJob:  jobID,
			payload: json.RawMessage("not json"),
			want:    false,
		},
	}

	// Add a test for invalid filter JSON (fail closed).
	t.Run("invalid filter JSON fails closed", func(t *testing.T) {
		p := models.NotificationPolicy{
			Filters: []byte("not valid json"),
		}
		evt := event.Event{JobID: jobID}
		got := policyFilterMatches(p, evt)
		if got != false {
			t.Error("expected false for invalid filter JSON")
		}
	})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var filtersJSON []byte
			if tt.filter != nil {
				filtersJSON, _ = json.Marshal(tt.filter)
			}
			p := models.NotificationPolicy{
				Filters: filtersJSON,
			}
			evt := event.Event{
				JobID:   tt.evtJob,
				Payload: tt.payload,
			}
			got := policyFilterMatches(p, evt)
			if got != tt.want {
				t.Errorf("policyFilterMatches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidEventTypes(t *testing.T) {
	valid := ValidEventTypes()

	expected := []event.Type{
		event.TypeTaskFailed,
		event.TypeRunFailed,
		event.TypeRunTimedOut,
		event.TypeSLAMissed,
		event.TypeRunCompleted,
		event.TypeTaskSucceeded,
	}

	for _, et := range expected {
		if _, ok := valid[et]; !ok {
			t.Errorf("expected %q in ValidEventTypes", et)
		}
	}

	if _, ok := valid[event.TypeJobCreated]; ok {
		t.Error("did not expect job_created in ValidEventTypes")
	}
}

func TestValidChannelTypes(t *testing.T) {
	valid := ValidChannelTypes()

	expected := []models.ChannelType{
		models.ChannelTypeWebhook,
		models.ChannelTypeSlack,
		models.ChannelTypeEmail,
		models.ChannelTypePagerDuty,
		models.ChannelTypeAIAgent,
	}

	for _, ct := range expected {
		if _, ok := valid[ct]; !ok {
			t.Errorf("expected %q in ValidChannelTypes", ct)
		}
	}
}

func TestDecodePolicyEventTypes(t *testing.T) {
	raw, _ := json.Marshal([]string{"task_failed", "run_failed", "sla_missed"})
	types, err := DecodePolicyEventTypes(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(types) != 3 {
		t.Fatalf("expected 3 types, got %d", len(types))
	}
	if types[0] != event.TypeTaskFailed {
		t.Errorf("expected %q, got %q", event.TypeTaskFailed, types[0])
	}
	if types[2] != event.TypeSLAMissed {
		t.Errorf("expected %q, got %q", event.TypeSLAMissed, types[2])
	}
}

func TestDecodePolicyEventTypesInvalid(t *testing.T) {
	_, err := DecodePolicyEventTypes([]byte("not json"))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestIsTimeoutError(t *testing.T) {
	tests := []struct {
		msg  string
		want bool
	}{
		{"run timed out after 30s", true},
		{"run timed out after 5m0s", true},
		{"exit code 1", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isTimeoutError(tt.msg); got != tt.want {
			t.Errorf("isTimeoutError(%q) = %v, want %v", tt.msg, got, tt.want)
		}
	}
}

// --- SLA config tests ---

func TestParseSLA(t *testing.T) {
	tests := []struct {
		name        string
		input       json.RawMessage
		wantNil     bool
		wantDur     time.Duration
		wantCompBy  string
	}{
		{
			name:    "nil input",
			input:   nil,
			wantNil: true,
		},
		{
			name:    "empty JSON",
			input:   json.RawMessage(`{}`),
			wantNil: true,
		},
		{
			name:    "duration only",
			input:   mustJSON(slaConfig{Duration: 30 * time.Minute}),
			wantDur: 30 * time.Minute,
		},
		{
			name:       "completedBy only",
			input:      mustJSON(slaConfig{CompletedBy: "09:00"}),
			wantCompBy: "09:00",
		},
		{
			name:       "both",
			input:      mustJSON(slaConfig{Duration: 1 * time.Hour, CompletedBy: "14:30"}),
			wantDur:    1 * time.Hour,
			wantCompBy: "14:30",
		},
		{
			name:    "invalid JSON",
			input:   json.RawMessage(`not json`),
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseSLA(tt.input)
			if tt.wantNil {
				if got != nil {
					t.Errorf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected non-nil SLA config")
			}
			if got.Duration != tt.wantDur {
				t.Errorf("duration: got %v, want %v", got.Duration, tt.wantDur)
			}
			if got.CompletedBy != tt.wantCompBy {
				t.Errorf("completedBy: got %q, want %q", got.CompletedBy, tt.wantCompBy)
			}
		})
	}
}

func TestResolveCompletedBy(t *testing.T) {
	refTime := time.Date(2026, 4, 13, 10, 30, 0, 0, time.UTC)

	tests := []struct {
		name    string
		hhmm    string
		wantH   int
		wantM   int
		wantErr bool
	}{
		{"morning", "09:00", 9, 0, false},
		{"afternoon", "14:30", 14, 30, false},
		{"midnight", "00:00", 0, 0, false},
		{"end of day", "23:59", 23, 59, false},
		{"invalid format", "9am", 0, 0, true},
		{"invalid hour", "25:00", 0, 0, true},
		{"invalid minute", "09:61", 0, 0, true},
		{"empty", "", 0, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveCompletedBy(tt.hhmm, refTime)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Hour() != tt.wantH || got.Minute() != tt.wantM {
				t.Errorf("got %02d:%02d, want %02d:%02d", got.Hour(), got.Minute(), tt.wantH, tt.wantM)
			}
			// Should be on the same date as refTime.
			if got.Year() != 2026 || got.Month() != 4 || got.Day() != 13 {
				t.Errorf("wrong date: got %s", got.Format("2006-01-02"))
			}
		})
	}
}

func mustJSON(v interface{}) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}
