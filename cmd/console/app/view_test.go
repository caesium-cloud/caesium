package app

import (
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/cmd/console/api"
)

func TestRenderFooterPreservesQuitHint(t *testing.T) {
	keys := []string{"[1/2/3] switch", "[tab] cycle", "[r] reload", "[p] ping", "[q] quit", "[T] theme", "[?] help"}
	status := "api:healthy  ping:22ms  load:27ms  retries:0  checked:19:48:58"

	footer := renderFooter(keys, status, 80)

	if !strings.Contains(footer, "[q] quit") {
		t.Fatalf("expected quit hint in footer, got: %q", footer)
	}
}

func TestJobsDetailFooterKeysAreContextAware(t *testing.T) {
	withoutLogs := jobsDetailFooterKeys(false)
	withLogs := jobsDetailFooterKeys(true)

	joinedWithout := strings.Join(withoutLogs, " ")
	joinedWith := strings.Join(withLogs, " ")

	if strings.Contains(joinedWithout, "[space] follow") {
		t.Fatalf("did not expect log controls in detail footer when logs are closed: %q", joinedWithout)
	}
	if !strings.Contains(joinedWith, "[space] follow") {
		t.Fatalf("expected log controls in detail footer when logs are open: %q", joinedWith)
	}
}

func TestRenderLogsModalEmptyStateCopy(t *testing.T) {
	model := New(nil)
	model.state = statusReady
	model.showLogsModal = true
	model.logTaskID = "task-1"
	model.logRunID = "run-1"
	model.viewportWidth = 120
	model.viewportHeight = 40

	out := model.renderLogsModal("")
	if !strings.Contains(out, "No log lines received yet for this task.") {
		t.Fatalf("expected updated empty logs message, got: %q", out)
	}
	if strings.Contains(out, "Press g to stream logs") {
		t.Fatalf("did not expect stale stream guidance in logs modal: %q", out)
	}
}

func TestThemeBadgeLabelUsesStableTag(t *testing.T) {
	if got := themeBadgeLabel("Ocean"); got != "OCEA" {
		t.Fatalf("expected OCEA theme tag, got %q", got)
	}
	bar := renderTabsBar(sectionJobs, 80, "Ocean")
	if strings.Contains(bar, "O...") {
		t.Fatalf("unexpected ellipsis in theme badge: %q", bar)
	}
	if !strings.Contains(bar, "OCEA") {
		t.Fatalf("expected OCEA label in theme badge: %q", bar)
	}
}

func TestRenderRunsModalHintShowsTerminalOnlyForRunning(t *testing.T) {
	model := New(nil)
	model.state = statusReady
	model.showRunsModal = true
	model.viewportWidth = 120
	model.viewportHeight = 40
	model.runs = []api.Run{{ID: "run-1", JobID: "job-1", Status: "running"}}
	model.runsTable.SetRows(runsToRows(model.runs, ""))
	model.runsTable.SetCursor(0)

	out := model.renderRunsModal("")
	if !strings.Contains(out, "terminal only") {
		t.Fatalf("expected terminal-only rerun hint for running run, got: %q", out)
	}
}
