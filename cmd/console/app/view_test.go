package app

import (
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/cmd/console/api"
)

func TestRenderFooterPreservesQuitHint(t *testing.T) {
	keys := []string{"[1/2/3/4] switch", "[tab] cycle", "[r] reload", "[p] ping", "[q] quit", "[T] theme", "[?] help"}
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

func TestTabsBarAlwaysShowsCsLogo(t *testing.T) {
	bar := renderTabsBar(sectionJobs, 80)
	if !strings.Contains(bar, "Cs") {
		t.Fatalf("expected Cs logo in tabs bar, got: %q", bar)
	}
}

func TestTabBarIncludesStatsTab(t *testing.T) {
	bar := renderTabs(sectionStats)
	if !strings.Contains(bar, "Stats") {
		t.Fatalf("expected Stats tab in tabs bar, got: %q", bar)
	}
	if !strings.Contains(bar, "4 Stats") {
		t.Fatalf("expected '4 Stats' label, got: %q", bar)
	}
}

func TestGlobalFooterKeysInclude4(t *testing.T) {
	keys := globalFooterKeys()
	joined := strings.Join(keys, " ")
	if !strings.Contains(joined, "1/2/3/4") {
		t.Fatalf("expected [1/2/3/4] in footer keys, got: %q", joined)
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

func TestConfirmModalDimensionsStayCompact(t *testing.T) {
	width, height := confirmModalDimensions(
		240,
		80,
		"Trigger Job",
		"Trigger fanout-join-demo now?",
	)

	if width > 72 {
		t.Fatalf("expected compact modal width <= 72, got %d", width)
	}
	if height > 12 {
		t.Fatalf("expected compact modal height <= 12, got %d", height)
	}
	if width < 36 || height < 8 {
		t.Fatalf("unexpectedly tiny confirm modal size: %dx%d", width, height)
	}
}

func TestOverlayCenteredKeepsBackgroundContent(t *testing.T) {
	background := strings.Join([]string{
		"TOP-LINE",
		"AAAAAAAAAA",
		"AAAAAAAAAA",
		"BOTTOM-LINE",
	}, "\n")

	out := overlayCentered(background, "XX", 10, 4)
	lines := strings.Split(out, "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "TOP-LINE") {
		t.Fatalf("expected top line preserved, got %q", lines[0])
	}
	if lines[1] != "AAAAXXAAAA" {
		t.Fatalf("expected centered overlay on line 2, got %q", lines[1])
	}
}

func TestRenderConfirmModalOverlaysOnBackground(t *testing.T) {
	model := New(nil)
	model.viewportWidth = 80
	model.viewportHeight = 24
	model.confirmAction = &actionRequest{
		kind:  actionTrigger,
		jobID: "job-1",
		label: "fanout-join-demo",
	}

	background := strings.Join([]string{
		"JOBS-LIST-BACKGROUND",
		"row one",
		"row two",
	}, "\n")

	out := model.renderConfirmModal(background)

	if !strings.Contains(out, "JOBS-LIST-BACKGROUND") {
		t.Fatalf("expected background content to remain visible, got: %q", out)
	}
	if !strings.Contains(out, "Trigger Job") {
		t.Fatalf("expected confirm modal title in output, got: %q", out)
	}
}
