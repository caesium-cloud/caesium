package main

import (
	"bytes"
	"os/exec"
	"testing"
	"time"
)

func TestSuperviseMirrorsWorkloadStatusAndOutlivesIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		code   int
		record string
	}{
		{"killed", "kill -9 $$", 137, "workload terminated by signal 9\n"},
		{"terminated", "kill -15 $$", 143, "workload terminated by signal 15\n"},
		{"exited", "exit 3", 3, "workload exited with status 3\n"},
		{"succeeded", "exit 0", 0, "workload exited with status 0\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const linger = 300 * time.Millisecond
			var out bytes.Buffer
			started := time.Now()
			code := supervise(exec.Command("/bin/sh", "-c", tc.script), linger, &out)
			elapsed := time.Since(started)
			if code != tc.code {
				t.Fatalf("exit code = %d, want %d", code, tc.code)
			}
			if out.String() != tc.record {
				t.Fatalf("record = %q, want %q", out.String(), tc.record)
			}
			// The supervisor must outlive its workload: that is what keeps the
			// container's cgroup populated after a kernel OOM kill.
			if elapsed < linger {
				t.Fatalf("supervisor exited after %s, before its %s linger", elapsed, linger)
			}
		})
	}
}

func TestSuperviseRefusesAWorkloadThatCannotStart(t *testing.T) {
	var out bytes.Buffer
	if code := supervise(exec.Command("/nonexistent/resource-stress"), time.Minute, &out); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if out.Len() != 0 {
		t.Fatalf("unexpected workload record %q", out.String())
	}
}
