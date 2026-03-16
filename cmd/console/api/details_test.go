package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/cmd/console/config"
)

func TestJobDetailFetchWithDAG(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/jobs/123":
			w.WriteHeader(http.StatusOK)
			w.Header().Set("Content-Type", "application/json")
			body := `{
				"id": "123",
				"alias": "nightly",
				"trigger_id": "trigger-9",
				"paused": true,
				"labels": {"env": "prod"},
				"annotations": {"owner": "ops"},
				"created_at": "2024-01-01T00:00:00Z",
				"updated_at": "2024-01-02T00:00:00Z",
				"trigger": {
					"id": "trigger-9",
					"alias": "nightly-cron",
					"type": "cron",
					"configuration": "@daily",
					"created_at": "2023-12-31T00:00:00Z",
					"updated_at": "2024-01-02T00:00:00Z"
				},
				"latest_run": {
					"id": "run-42",
					"job_id": "123",
					"status": "running",
					"started_at": "2024-01-02T12:00:00Z",
					"tasks": []
				}
			}`
			if _, err := w.Write([]byte(body)); err != nil {
				t.Fatalf("write job detail: %v", err)
			}
		case "/v1/jobs/123/dag":
			w.WriteHeader(http.StatusOK)
			w.Header().Set("Content-Type", "application/json")
			body := `{
				"job_id": "123",
				"nodes": [
					{"id": "task-a", "atom_id": "atom-1", "successors": ["task-b"]},
					{"id": "task-b", "atom_id": "atom-2", "successors": []}
				],
				"edges": [{"from": "task-a", "to": "task-b"}]
			}`
			if _, err := w.Write([]byte(body)); err != nil {
				t.Fatalf("write dag: %v", err)
			}
		case "/v1/jobs/123/tasks":
			w.WriteHeader(http.StatusOK)
			w.Header().Set("Content-Type", "application/json")
			body := `[
				{"id":"task-a","job_id":"123","atom_id":"atom-1","retries":2,"retry_delay":1000000000,"retry_backoff":true,"trigger_rule":"one_success","created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-02T00:00:00Z"},
				{"id":"task-b","job_id":"123","atom_id":"atom-2","retries":0,"retry_delay":0,"retry_backoff":false,"trigger_rule":"all_success","created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-02T00:00:00Z"}
			]`
			if _, err := w.Write([]byte(body)); err != nil {
				t.Fatalf("write tasks: %v", err)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	cfg := &config.Config{
		BaseURL:     mustParse(t, ts.URL),
		HTTPTimeout: time.Second,
	}
	client := New(cfg)

	detail, err := client.Jobs().Detail(context.Background(), "123", &JobDetailOptions{IncludeDAG: true})
	if err != nil {
		t.Fatalf("Detail returned error: %v", err)
	}

	if detail.Job.ID != "123" {
		t.Fatalf("expected job id 123, got %s", detail.Job.ID)
	}
	if !detail.Job.Paused {
		t.Fatal("expected paused job state to be loaded")
	}

	if detail.Trigger == nil || detail.Trigger.ID != "trigger-9" {
		t.Fatalf("expected trigger id trigger-9, got %#v", detail.Trigger)
	}

	if detail.LatestRun == nil || detail.LatestRun.ID != "run-42" {
		t.Fatalf("expected latest run run-42, got %#v", detail.LatestRun)
	}

	if detail.DAG == nil {
		t.Fatal("expected DAG to be loaded")
	}

	if len(detail.DAG.Nodes) != 2 {
		t.Fatalf("expected 2 DAG nodes, got %d", len(detail.DAG.Nodes))
	}
	if len(detail.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(detail.Tasks))
	}
}
