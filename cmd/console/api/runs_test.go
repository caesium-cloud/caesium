package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/cmd/console/config"
)

func TestRunsListAndGet(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/123/runs":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if _, err := w.Write([]byte(`[
				{"id":"r1","job_id":"123","status":"running","started_at":"2024-01-01T00:00:00Z","tasks":[{"id":"t1","status":"running","claimed_by":"node-a","claim_attempt":2}]}
			]`)); err != nil {
				t.Fatalf("write response: %v", err)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/123/runs/r1":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if _, err := w.Write([]byte(`{"id":"r1","job_id":"123","status":"running","started_at":"2024-01-01T00:00:00Z","tasks":[]}`)); err != nil {
				t.Fatalf("write response: %v", err)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1/jobs/123/run":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			if _, err := w.Write([]byte(`{"id":"r2","job_id":"123","status":"running","started_at":"2024-01-02T00:00:00Z","tasks":[]}`)); err != nil {
				t.Fatalf("write response: %v", err)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	cfg := &config.Config{BaseURL: mustParse(t, ts.URL), HTTPTimeout: time.Second}
	client := New(cfg)

	list, err := client.Runs().List(context.Background(), "123", url.Values{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(list) != 1 {
		t.Fatalf("expected 1 run, got %d", len(list))
	}
	if len(list[0].Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(list[0].Tasks))
	}
	if list[0].Tasks[0].ClaimedBy != "node-a" {
		t.Fatalf("expected claimed_by to be 'node-a', got %q", list[0].Tasks[0].ClaimedBy)
	}
	if list[0].Tasks[0].ClaimAttempt != 2 {
		t.Fatalf("expected claim_attempt=2, got %d", list[0].Tasks[0].ClaimAttempt)
	}

	detail, err := client.Runs().Get(context.Background(), "123", "r1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if detail.ID != "r1" {
		t.Fatalf("unexpected run id %q", detail.ID)
	}

	triggered, err := client.Runs().Trigger(context.Background(), "123")
	if err != nil {
		t.Fatalf("unexpected trigger error: %v", err)
	}

	if triggered.ID != "r2" {
		t.Fatalf("unexpected triggered run id %q", triggered.ID)
	}
}
