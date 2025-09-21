package app

import (
	"testing"

	"github.com/caesium-cloud/caesium/cmd/console/api"
)

func TestJobsToRowsIncludesMetadata(t *testing.T) {
	jobs := []api.Job{{
		Alias: "nightly",
		ID:    "job-123",
		Labels: map[string]string{
			"env":  "prod",
			"team": "data",
		},
		Annotations: map[string]string{
			"owner": "ops",
		},
	}}

	rows := jobsToRows(jobs)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}

	row := rows[0]
	if got, want := row[1], "env=prod, team=data"; got != want {
		t.Fatalf("labels column = %q, want %q", got, want)
	}

	if got, want := row[2], "owner=ops"; got != want {
		t.Fatalf("annotations column = %q, want %q", got, want)
	}
}

func TestFormatStringMapEmpty(t *testing.T) {
	if got := formatStringMap(nil); got != "-" {
		t.Fatalf("nil map -> %q, want '-'", got)
	}

	if got := formatStringMap(map[string]string{}); got != "-" {
		t.Fatalf("empty map -> %q, want '-'", got)
	}
}
