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

func TestJobsList(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		if got := r.URL.Query().Get("order_by"); got != "created_at" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(`[
			{"id":"1","alias":"job-a","labels":{"env":"prod"},"annotations":{"owner":"ops"},"created_at":"2024-01-01T00:00:00Z"},
			{"id":"2","alias":"job-b","created_at":"2024-01-02T00:00:00Z"}
		]`)); err != nil {
			t.Fatalf("write response: %v", err)
		}
	}))

	defer ts.Close()

	cfg := &config.Config{BaseURL: mustParse(t, ts.URL), HTTPTimeout: time.Second}
	client := New(cfg)

	jobs, err := client.Jobs().List(context.Background(), url.Values{"order_by": []string{"created_at"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(jobs))
	}

	if jobs[0].Alias != "job-a" {
		t.Fatalf("expected first alias job-a, got %s", jobs[0].Alias)
	}

	if got := jobs[0].Labels["env"]; got != "prod" {
		t.Fatalf("expected label env=prod, got %s", got)
	}

	if got := jobs[0].Annotations["owner"]; got != "ops" {
		t.Fatalf("expected annotation owner=ops, got %s", got)
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	return u
}
