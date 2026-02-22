package lineage

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testEvent() RunEvent {
	return RunEvent{
		EventTime: time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC),
		EventType: EventTypeStart,
		Producer:  producerURI,
		SchemaURL: schemaURL,
		Run: Run{
			RunID: uuid.MustParse("d46e465b-d358-4d32-83d4-df660ff614dd"),
		},
		Job: Job{
			Namespace: "caesium-test",
			Name:      "test_job",
		},
		Inputs:  []Dataset{},
		Outputs: []Dataset{},
	}
}

func TestHTTPTransport(t *testing.T) {
	var receivedBody []byte
	var receivedContentType string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		receivedBody = body
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	transport := NewHTTPTransport(HTTPTransportConfig{
		URL:     server.URL,
		Headers: map[string]string{"X-Custom": "test-value"},
		Timeout: 5 * time.Second,
	})

	err := transport.Emit(context.Background(), testEvent())
	if err != nil {
		t.Fatalf("emit: %v", err)
	}

	if receivedContentType != "application/json" {
		t.Errorf("content-type = %v, want application/json", receivedContentType)
	}

	var parsed RunEvent
	if err := json.Unmarshal(receivedBody, &parsed); err != nil {
		t.Fatalf("unmarshal received body: %v", err)
	}

	if parsed.EventType != EventTypeStart {
		t.Errorf("eventType = %v, want START", parsed.EventType)
	}
	if parsed.Job.Name != "test_job" {
		t.Errorf("job.name = %v, want test_job", parsed.Job.Name)
	}
}

func TestHTTPTransportError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	transport := NewHTTPTransport(HTTPTransportConfig{URL: server.URL})

	err := transport.Emit(context.Background(), testEvent())
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %v, want to contain '500'", err)
	}
}

func TestFileTransport(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "lineage-test-*.ndjson")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	tmpPath := tmpFile.Name()
	if err := tmpFile.Close(); err != nil {
		t.Fatalf("close temp file: %v", err)
	}
	defer func() {
		if err := os.Remove(tmpPath); err != nil {
			t.Fatalf("remove temp file: %v", err)
		}
	}()

	transport, err := NewFileTransport(tmpPath)
	if err != nil {
		t.Fatalf("create file transport: %v", err)
	}

	event1 := testEvent()
	event2 := testEvent()
	event2.EventType = EventTypeComplete

	if err := transport.Emit(context.Background(), event1); err != nil {
		t.Fatalf("emit event1: %v", err)
	}
	if err := transport.Emit(context.Background(), event2); err != nil {
		t.Fatalf("emit event2: %v", err)
	}
	if err := transport.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	data, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}

	var parsed1 RunEvent
	if err := json.Unmarshal([]byte(lines[0]), &parsed1); err != nil {
		t.Fatalf("unmarshal line 1: %v", err)
	}
	if parsed1.EventType != EventTypeStart {
		t.Errorf("line 1 eventType = %v, want START", parsed1.EventType)
	}

	var parsed2 RunEvent
	if err := json.Unmarshal([]byte(lines[1]), &parsed2); err != nil {
		t.Fatalf("unmarshal line 2: %v", err)
	}
	if parsed2.EventType != EventTypeComplete {
		t.Errorf("line 2 eventType = %v, want COMPLETE", parsed2.EventType)
	}
}

type recordingTransport struct {
	events []RunEvent
	err    error
}

func (t *recordingTransport) Emit(_ context.Context, event RunEvent) error {
	t.events = append(t.events, event)
	return t.err
}

func (t *recordingTransport) Close() error { return nil }

func TestCompositeTransport(t *testing.T) {
	t1 := &recordingTransport{}
	t2 := &recordingTransport{}

	composite := NewCompositeTransport(t1, t2)

	event := testEvent()
	if err := composite.Emit(context.Background(), event); err != nil {
		t.Fatalf("emit: %v", err)
	}

	if len(t1.events) != 1 {
		t.Errorf("t1 received %d events, want 1", len(t1.events))
	}
	if len(t2.events) != 1 {
		t.Errorf("t2 received %d events, want 1", len(t2.events))
	}
}

func TestCompositeTransportAggregatesErrors(t *testing.T) {
	t1 := &recordingTransport{err: errors.New("t1 failed")}
	t2 := &recordingTransport{err: errors.New("t2 failed")}

	composite := NewCompositeTransport(t1, t2)

	err := composite.Emit(context.Background(), testEvent())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "t1 failed") {
		t.Errorf("error should contain 't1 failed': %v", err)
	}
	if !strings.Contains(err.Error(), "t2 failed") {
		t.Errorf("error should contain 't2 failed': %v", err)
	}
}
