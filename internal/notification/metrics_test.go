package notification

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/event"
)

func TestRecordEventMetric(t *testing.T) {
	// Just verify it doesn't panic on all event types.
	events := []event.Event{
		{Type: event.TypeTaskFailed, Payload: mustJSON(map[string]string{"job_alias": "a"})},
		{Type: event.TypeRunFailed, Payload: mustJSON(map[string]string{"job_alias": "b"})},
		{Type: event.TypeRunTimedOut, Payload: mustJSON(map[string]string{"job_alias": "c"})},
		{Type: event.TypeSLAMissed, Payload: mustJSON(map[string]string{"job_alias": "d"})},
		{Type: event.TypeRunCompleted},
		{Type: event.TypeTaskSucceeded},
	}
	for _, evt := range events {
		recordEventMetric(evt)
	}
}

func TestExtractJobAlias(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    string
	}{
		{"with alias", mustJSON(map[string]string{"job_alias": "pipeline"}), "pipeline"},
		{"empty payload", nil, ""},
		{"no alias key", mustJSON(map[string]string{"foo": "bar"}), ""},
		{"invalid json", []byte("bad"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evt := event.Event{Payload: tt.payload}
			got := extractJobAlias(evt)
			if got != tt.want {
				t.Errorf("extractJobAlias() = %q, want %q", got, tt.want)
			}
		})
	}
}
