package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/cmd/console/config"
)

func TestStatsGet(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/v1/stats" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(`{
			"jobs": {
				"total": 5,
				"recent_runs": 12,
				"success_rate": 0.85,
				"avg_duration_seconds": 42.5
			},
			"top_failing": [
				{"job_id": "j1", "alias": "etl", "failure_count": 3, "last_failure": "2024-06-01T10:00:00Z"}
			],
			"slowest_jobs": [
				{"job_id": "j2", "alias": "export", "avg_duration_seconds": 300.0}
			]
		}`)); err != nil {
			t.Fatalf("write response: %v", err)
		}
	}))
	defer ts.Close()

	cfg := &config.Config{BaseURL: mustParse(t, ts.URL), HTTPTimeout: time.Second}
	client := New(cfg)

	stats, err := client.Stats().Get(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stats.Jobs.Total != 5 {
		t.Fatalf("expected total 5, got %d", stats.Jobs.Total)
	}
	if stats.Jobs.RecentRuns != 12 {
		t.Fatalf("expected recent_runs 12, got %d", stats.Jobs.RecentRuns)
	}
	if stats.Jobs.SuccessRate != 0.85 {
		t.Fatalf("expected success_rate 0.85, got %f", stats.Jobs.SuccessRate)
	}
	if stats.Jobs.AvgDurationSeconds != 42.5 {
		t.Fatalf("expected avg_duration 42.5, got %f", stats.Jobs.AvgDurationSeconds)
	}

	if len(stats.TopFailing) != 1 {
		t.Fatalf("expected 1 top failing, got %d", len(stats.TopFailing))
	}
	if stats.TopFailing[0].Alias != "etl" {
		t.Fatalf("expected alias etl, got %s", stats.TopFailing[0].Alias)
	}
	if stats.TopFailing[0].FailureCount != 3 {
		t.Fatalf("expected failure_count 3, got %d", stats.TopFailing[0].FailureCount)
	}

	if len(stats.SlowestJobs) != 1 {
		t.Fatalf("expected 1 slowest job, got %d", len(stats.SlowestJobs))
	}
	if stats.SlowestJobs[0].Alias != "export" {
		t.Fatalf("expected alias export, got %s", stats.SlowestJobs[0].Alias)
	}
}

func TestStatsGetServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	cfg := &config.Config{BaseURL: mustParse(t, ts.URL), HTTPTimeout: time.Second}
	client := New(cfg)

	_, err := client.Stats().Get(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}
