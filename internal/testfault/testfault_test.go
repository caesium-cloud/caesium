//go:build testfault

package testfault_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/testfault"
)

func writeDirective(t *testing.T, dir string, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, testfault.PauseFile), []byte(body), 0o600); err != nil {
		t.Fatalf("write directive: %v", err)
	}
}

func readLog(t *testing.T, dir string) []map[string]any {
	t.Helper()
	fh, err := os.Open(filepath.Join(dir, testfault.LogFile))
	if err != nil {
		return nil
	}
	defer func() { _ = fh.Close() }()
	var out []map[string]any
	scanner := bufio.NewScanner(fh)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("hook log line is not JSON: %v (%s)", err, line)
		}
		out = append(out, rec)
	}
	return out
}

// TestArmedHookHoldsUntilDisarmed proves the tagged build actually blocks the
// publication of a matching event and records its own entry, which is the
// evidence the robustness test reads back — not an inferred delivery gap.
func TestArmedHookHoldsUntilDisarmed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(testfault.DirEnv, dir)
	writeDirective(t, dir, `{"armed":true,"run_id":"run-1","max_hold_ms":30000}`)

	done := make(chan struct{})
	go func() {
		defer close(done)
		testfault.BeforeBusPublish(context.Background(), testfault.PathDispatchOnce, "task_started", "run-1", 11)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if entries := readLog(t, dir); len(entries) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hook never recorded an entry for an armed run")
		}
		time.Sleep(20 * time.Millisecond)
	}

	select {
	case <-done:
		t.Fatal("hook returned while still armed")
	case <-time.After(300 * time.Millisecond):
	}

	if err := os.Remove(filepath.Join(dir, testfault.PauseFile)); err != nil {
		t.Fatalf("disarm: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hook did not release after disarm")
	}

	entries := readLog(t, dir)
	if len(entries) < 2 {
		t.Fatalf("expected enter and release records, got %d", len(entries))
	}
	if entries[0]["phase"] != "enter" || entries[len(entries)-1]["phase"] != "release" {
		t.Fatalf("unexpected hook phases: %v", entries)
	}
	if entries[0]["banner"] != testfault.ControlBanner {
		t.Fatalf("missing control banner: %v", entries[0])
	}
	if entries[0]["path"] != testfault.PathDispatchOnce {
		t.Fatalf("hook did not record the publication path: %v", entries[0])
	}
}

// TestUnmatchedEventIsNotHeld keeps the predicate keyed by run/event: an armed
// directive for one run must not stall every other publication in the process.
func TestUnmatchedEventIsNotHeld(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(testfault.DirEnv, dir)
	writeDirective(t, dir, `{"armed":true,"run_id":"run-1","types":["task_started"],"max_hold_ms":30000}`)

	start := time.Now()
	testfault.BeforeBusPublish(context.Background(), testfault.PathPublishAndMark, "job_created", "run-2", 2)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("unmatched event was held for %s", elapsed)
	}
}

// TestMaxHoldBoundsTheFault keeps an armed hook from wedging a server forever.
func TestMaxHoldBoundsTheFault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(testfault.DirEnv, dir)
	writeDirective(t, dir, `{"armed":true,"max_hold_ms":300}`)

	start := time.Now()
	testfault.BeforeBusPublish(context.Background(), testfault.PathPublishAndMark, "task_started", "run-9", 3)
	elapsed := time.Since(start)
	if elapsed < 250*time.Millisecond {
		t.Fatalf("hook did not hold: %s", elapsed)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("hook ignored max_hold_ms: %s", elapsed)
	}
	entries := readLog(t, dir)
	if len(entries) == 0 || entries[len(entries)-1]["reason"] != "max_hold" {
		t.Fatalf("expected a max_hold release record, got %v", entries)
	}
}

// TestContextCancellationReleases proves a cancelled request does not stay
// wedged in the hook.
func TestContextCancellationReleases(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(testfault.DirEnv, dir)
	writeDirective(t, dir, `{"armed":true,"max_hold_ms":60000}`)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		testfault.BeforeBusPublish(ctx, testfault.PathDispatchOnce, "task_started", "run-3", 4)
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hook ignored context cancellation")
	}
}

// TestInertWithoutControlDir proves the instrumented binary is still inert
// unless a test explicitly points it at a control directory.
func TestInertWithoutControlDir(t *testing.T) {
	t.Setenv(testfault.DirEnv, "")
	start := time.Now()
	testfault.BeforeBusPublish(context.Background(), testfault.PathDispatchOnce, "task_started", "run-4", 5)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("instrumented build blocked with no control directory: %s", elapsed)
	}
}
