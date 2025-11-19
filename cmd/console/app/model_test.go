package app

import (
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/cmd/console/api"
	"github.com/caesium-cloud/caesium/cmd/console/config"
	"github.com/caesium-cloud/caesium/cmd/console/ui/dag"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
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

func TestJobDetailLoadedBuildsGraph(t *testing.T) {
	model := New(nil)
	model.state = statusReady
	detail := &api.JobDetail{
		Job: api.JobDescriptor{ID: "job-1", Alias: "demo"},
		DAG: &api.JobDAG{
			Nodes: []api.JobDAGNode{{ID: "task-a", AtomID: "atom-1"}},
		},
	}

	res, cmd := model.Update(jobDetailLoadedMsg{detail: detail})
	if cmd != nil {
		t.Fatalf("expected no command, got %#v", cmd)
	}

	updated := res.(Model)
	if updated.graph == nil {
		t.Fatal("expected graph to be populated")
	}
	if updated.focusedNodeID != "task-a" {
		t.Fatalf("focused node = %s, want task-a", updated.focusedNodeID)
	}
	if updated.detailErr != nil {
		t.Fatalf("detailErr should be nil, got %v", updated.detailErr)
	}
}

func TestJobDetailErrorSetsDetailError(t *testing.T) {
	model := New(nil)
	model.state = statusReady

	err := errors.New("boom")
	res, _ := model.Update(jobDetailErrMsg{err: err})
	updated := res.(Model)
	if !errors.Is(updated.detailErr, err) {
		t.Fatalf("detailErr = %v, want %v", updated.detailErr, err)
	}
	if updated.graph != nil {
		t.Fatal("graph should be nil after error")
	}
}

func TestSetFocusedNodePreloadsAtomMetadata(t *testing.T) {
	cfg := &config.Config{BaseURL: mustParseURL(t, "http://example.com"), HTTPTimeout: time.Second}
	client := api.New(cfg)

	model := New(client)
	graph, err := dag.FromJobDAG(&api.JobDAG{Nodes: []api.JobDAGNode{{ID: "task-a", AtomID: "atom-1"}}})
	if err != nil {
		t.Fatalf("graph build failed: %v", err)
	}
	model.graph = graph

	cmd := model.setFocusedNode("task-a")
	if cmd == nil {
		t.Fatal("expected command to fetch atom metadata")
	}
	if model.loadingAtomID != "atom-1" {
		t.Fatalf("loadingAtomID = %s, want atom-1", model.loadingAtomID)
	}

	res, _ := model.Update(atomDetailLoadedMsg{id: "atom-1", atom: &api.Atom{ID: "atom-1"}})
	updated := res.(Model)
	if updated.loadingAtomID != "" {
		t.Fatalf("loadingAtomID should reset, got %s", updated.loadingAtomID)
	}
	if _, ok := updated.atomDetails["atom-1"]; !ok {
		t.Fatal("atom detail should be cached")
	}
}

func TestAtomDetailErrorSetsAtomErr(t *testing.T) {
	model := New(nil)
	model.loadingAtomID = "atom-1"

	res, _ := model.Update(atomDetailErrMsg{id: "atom-1", err: errors.New("fail")})
	updated := res.(Model)
	if updated.loadingAtomID != "" {
		t.Fatal("expected loadingAtomID to clear")
	}
	if updated.atomErr == nil {
		t.Fatal("expected atomErr to be set")
	}
}

func TestEnterActivatesDetailView(t *testing.T) {
	model := New(nil)
	model.state = statusReady
	model.active = sectionJobs
	model.jobDetail = &api.JobDetail{Job: api.JobDescriptor{ID: "job-1", Alias: "demo"}}

	res, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	updated := res.(Model)
	if !updated.showDetail {
		t.Fatal("expected showDetail to be true after pressing enter")
	}
	if updated.detailLoading {
		t.Fatal("detailLoading should be false when detail already present")
	}
}

func TestEnterTriggersDetailFetchWhenMissing(t *testing.T) {
	model := New(nil)
	model.state = statusReady
	model.active = sectionJobs
	model.jobs.SetRows([]table.Row{{"alias", "-", "-", "job-1", ""}})

	res, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected command to fetch detail")
	}
	updated := res.(Model)
	if !updated.showDetail {
		t.Fatal("expected detail mode to activate")
	}
	if !updated.detailLoading {
		t.Fatal("expected detailLoading to be true while fetching")
	}
}

func TestEscExitsDetailView(t *testing.T) {
	model := New(nil)
	model.state = statusReady
	model.active = sectionJobs
	model.jobDetail = &api.JobDetail{Job: api.JobDescriptor{ID: "job-1"}}
	model.showDetail = true

	res, _ := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	updated := res.(Model)
	if updated.showDetail {
		t.Fatal("expected showDetail to be false after esc")
	}
}

func TestQExitsDetailView(t *testing.T) {
	model := New(nil)
	model.state = statusReady
	model.active = sectionJobs
	model.jobDetail = &api.JobDetail{Job: api.JobDescriptor{ID: "job-1"}}
	model.showDetail = true

	res, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	updated := res.(Model)
	if updated.showDetail {
		t.Fatal("expected showDetail to be false after q")
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	return u
}
