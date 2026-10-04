//go:build integration

package test

import (
	"os/exec"
	"testing"
	"time"
)

func TestJoinedCommandWaitsAndJoinsChildOutput(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "printf child-output; exit 7")
	out := &syncBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out
	child, err := startJoinedCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	waitErr, finished := child.waitFor(5 * time.Second)
	if !finished {
		child.killAndWait()
		t.Fatal("child did not exit before the deadline")
	}
	if waitErr == nil {
		t.Fatal("child exit status was lost")
	}
	if got := out.String(); got != "child-output" {
		t.Fatalf("joined output = %q, want child-output", got)
	}
	if code := cmd.ProcessState.ExitCode(); code != 7 {
		t.Fatalf("child exit code = %d, want 7", code)
	}
	child.killAndWait() // A consumed result must not be received a second time.
}

func TestJoinedCommandCleanupKillsAndReapsChild(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "sleep 60")
	child, err := startJoinedCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	child.killAndWait()
	if cmd.ProcessState == nil {
		t.Fatal("cleanup returned before the child was reaped")
	}
}
