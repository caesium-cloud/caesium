package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunWithSignalContextHelperProcess(t *testing.T) {
	mode := os.Getenv("REAGENTS_SIGNAL_TEST_MODE")
	if mode == "" {
		return
	}
	if mode == "child" {
		if err := os.WriteFile(os.Getenv("REAGENTS_SIGNAL_TEST_READY"), nil, 0o600); err != nil {
			os.Exit(2)
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	RunWithSignalContext("signal-role", func(ctx context.Context, e *Emitter) error {
		if err := e.Output(map[string]string{"staged": "value"}); err != nil {
			return err
		}
		if mode == "success" {
			fmt.Fprintln(os.Stderr, "role diagnostic")
			return nil
		}
		child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRunWithSignalContextHelperProcess$")
		child.Env = append(os.Environ(), "REAGENTS_SIGNAL_TEST_MODE=child")
		err := child.Run()
		if ctx.Err() == nil {
			return fmt.Errorf("child exited before cancellation: %v", err)
		}
		if err == nil {
			return fmt.Errorf("cancelled child unexpectedly succeeded")
		}
		if err := os.WriteFile(os.Getenv("REAGENTS_SIGNAL_TEST_CANCELLED"), []byte(fmt.Sprint(child.ProcessState.ExitCode())), 0o600); err != nil {
			return err
		}
		if mode == "force" {
			fmt.Fprintln(os.Stderr, "cancelled work winding down")
			for {
				time.Sleep(time.Hour)
			}
		}
		if mode == "error" {
			return fmt.Errorf("specific role failure")
		}
		return nil // The wrapper must turn ctx.Err into a fail-closed result.
	})
	os.Exit(0)
}

type signalProcess struct {
	cmd              *exec.Cmd
	stdout, stderr   bytes.Buffer
	done             chan struct{}
	err              error
	ready, cancelled string
}

func startSignalProcess(t *testing.T, mode string) *signalProcess {
	t.Helper()
	dir := t.TempDir()
	p := &signalProcess{done: make(chan struct{}), ready: filepath.Join(dir, "ready"), cancelled: filepath.Join(dir, "cancelled")}
	p.cmd = exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRunWithSignalContextHelperProcess$")
	p.cmd.Env = append(os.Environ(), "REAGENTS_SIGNAL_TEST_MODE="+mode, "REAGENTS_SIGNAL_TEST_READY="+p.ready, "REAGENTS_SIGNAL_TEST_CANCELLED="+p.cancelled)
	p.cmd.Stdout, p.cmd.Stderr = &p.stdout, &p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.err = p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		_ = p.cmd.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			t.Error("signal subprocess did not join")
		}
	})
	return p
}

func (p *signalProcess) wait(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("signal subprocess did not exit")
	}
}

func (p *signalProcess) waitFile(t *testing.T, path string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-p.done:
			t.Fatalf("subprocess exited before %s: %v; stderr=%s", filepath.Base(path), p.err, p.stderr.String())
		case <-timer.C:
			t.Fatalf("subprocess never wrote %s", filepath.Base(path))
		case <-ticker.C:
		}
	}
}

func TestRunWithSignalContextSuccessKeepsStreamsSeparate(t *testing.T) {
	p := startSignalProcess(t, "success")
	p.wait(t)
	if p.err != nil {
		t.Fatalf("success: %v: %s", p.err, p.stderr.String())
	}
	var values map[string]string
	line := strings.TrimSpace(p.stdout.String())
	if !strings.HasPrefix(line, OutputMarker+" ") {
		t.Fatalf("stdout = %q", line)
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, OutputMarker+" ")), &values); err != nil {
		t.Fatal(err)
	}
	if values["staged"] != "value" || !strings.Contains(p.stderr.String(), "role diagnostic") {
		t.Fatal("success lost marker or diagnostic")
	}
}

func TestRunWithSignalContextCancelsChildAndFailsClosed(t *testing.T) {
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			p := startSignalProcess(t, "cancel")
			p.waitFile(t, p.ready)
			if err := p.cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			p.wait(t)
			if p.err == nil || p.cmd.ProcessState.ExitCode() != 1 {
				t.Fatalf("first signal exit: %v", p.err)
			}
			if p.stdout.Len() != 0 {
				t.Fatalf("cancelled role published staged markers: %s", p.stdout.String())
			}
			if !strings.Contains(p.stderr.String(), "signal-role: context canceled") {
				t.Fatalf("stderr = %s", p.stderr.String())
			}
			exit, err := os.ReadFile(p.cancelled)
			if err != nil || string(exit) == "0" {
				t.Fatalf("direct child cancellation proof = %q, %v", exit, err)
			}
		})
	}
}

func TestRunWithSignalContextSecondSignalForcesTermination(t *testing.T) {
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			p := startSignalProcess(t, "force")
			p.waitFile(t, p.ready)
			if err := p.cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			p.waitFile(t, p.cancelled) // The direct child exited; role deliberately stays alive.
			if err := p.cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			p.wait(t)
			if p.err == nil || p.cmd.ProcessState.ExitCode() != -1 {
				t.Fatalf("second signal did not force termination: %v", p.err)
			}
			if p.stdout.Len() != 0 {
				t.Fatalf("force stop published staged markers: %s", p.stdout.String())
			}

		})
	}
}

func TestRunWithSignalContextKeepsRoleFailureOnCancellation(t *testing.T) {
	p := startSignalProcess(t, "error")
	p.waitFile(t, p.ready)
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	p.wait(t)
	if p.err == nil || p.cmd.ProcessState.ExitCode() != 1 || p.stdout.Len() != 0 {
		t.Fatalf("role failure = %v, stdout=%s", p.err, p.stdout.String())
	}
	if !strings.Contains(p.stderr.String(), "signal-role: specific role failure") || strings.Contains(p.stderr.String(), "context canceled") {
		t.Fatalf("role failure replaced: %s", p.stderr.String())
	}
}
