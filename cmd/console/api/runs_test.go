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
				{"id":"r1","job_id":"123","status":"running","started_at":"2024-01-01T00:00:00Z","tasks":[]}
			]`)); err != nil {
				t.Fatalf("write response: %v", err)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/123/runs/r1":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if _, err := w.Write([]byte(`{"id":"r1","job_id":"123","status":"running","started_at":"2024-01-01T00:00:00Z","tasks":[]}`)); err != nil {
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

	detail, err := client.Runs().Get(context.Background(), "123", "r1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if detail.ID != "r1" {
		t.Fatalf("unexpected run id %q", detail.ID)
	}
}
