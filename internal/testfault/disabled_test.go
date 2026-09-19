//go:build !testfault

package testfault_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/testfault"
)

// TestReleaseTwinIsInert is the untagged (release) guard: even with a control
// directory and a fully armed directive present on disk, the release build must
// not observe them. If this ever blocks or the constant flips, a release binary
// has gained a fault control.
func TestReleaseTwinIsInert(t *testing.T) {
	if testfault.Enabled {
		t.Fatal("testfault.Enabled is true in an untagged build")
	}

	dir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(dir, "bus-publish-pause.json"),
		[]byte(`{"armed":true,"max_hold_ms":600000}`),
		0o600,
	); err != nil {
		t.Fatalf("write directive: %v", err)
	}
	t.Setenv("CAESIUM_TESTFAULT_DIR", dir)

	done := make(chan struct{})
	go func() {
		defer close(done)
		testfault.BeforeBusPublish(context.Background(), "dispatch_once", "task_started", "run-1", 7)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("release BeforeBusPublish blocked on an armed directive")
	}

	if entries, err := os.ReadDir(dir); err != nil {
		t.Fatalf("read control dir: %v", err)
	} else if len(entries) != 1 {
		t.Fatalf("release build touched the control directory: %d entries", len(entries))
	}
}
