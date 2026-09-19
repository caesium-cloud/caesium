//go:build testfault

// Package testfault holds the single A1-approved, build-tag-gated test-only
// fault control. This file is compiled ONLY when the `testfault` build tag is
// supplied (see `build/Dockerfile.robustness`, target `instrumented-server`).
// The ordinary release image is built without the tag and gets `disabled.go`,
// whose Enabled constant is false, so every call site is removed at compile
// time and none of the strings below exist in the release binary.
//
// Control surface: a plain file on a test-owned volume, read by the server
// process itself. There is deliberately NO listener, port, route or token here:
// an unauthenticated product fault endpoint is forbidden by A1, and an
// authenticated one would add a second auth surface to get wrong. The directory
// is named by CAESIUM_TESTFAULT_DIR and is unset by default, so even the
// instrumented binary is inert unless a test explicitly points it at a
// writable, test-owned path (the robustness harness uses the per-member dqlite
// data volume created by its own Helm release).
//
// The directive is re-read on each consultation rather than cached in a
// background watcher: arming and disarming are then immediate and deterministic,
// and there is no goroutine whose lifetime outlives the fault. Execution-event
// volume is small and this code only ever runs in the instrumented image.
package testfault

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Enabled reports whether the test-only fault control is compiled in.
const Enabled = true

const (
	// ControlBanner is a distinctive marker string. `scripts/robustness.sh`
	// greps the release binary for it and refuses to deploy a release image
	// that contains it.
	ControlBanner = "caesium-testfault-control"

	// DirEnv names the directory holding the control files. Unset = inert.
	DirEnv = "CAESIUM_TESTFAULT_DIR"

	// PauseFile is the arming directive. Absent or armed=false means disarmed.
	PauseFile = "bus-publish-pause.json"

	// LogFile is the append-only JSONL record of hook entries and releases.
	LogFile = "bus-publish-hook.log"

	defaultMaxHold = 60 * time.Second
	holdPollPeriod = 50 * time.Millisecond
)

// Directive is the on-disk arming record.
type Directive struct {
	Armed     bool     `json:"armed"`
	RunID     string   `json:"run_id,omitempty"`
	Types     []string `json:"types,omitempty"`
	Paths     []string `json:"paths,omitempty"`
	MaxHoldMS int      `json:"max_hold_ms,omitempty"`
}

type entry struct {
	Banner   string `json:"banner"`
	At       string `json:"at"`
	Phase    string `json:"phase"`
	Path     string `json:"path"`
	Type     string `json:"type"`
	RunID    string `json:"run_id,omitempty"`
	Sequence uint64 `json:"sequence"`
	PID      int    `json:"pid"`
	Node     string `json:"node,omitempty"`
	Host     string `json:"host,omitempty"`
	HeldMS   int64  `json:"held_ms,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

var (
	logMu    sync.Mutex
	nodeOnce sync.Once
	nodeAddr string
	hostName string
)

func controlDir() string {
	return os.Getenv(DirEnv)
}

func identity() (string, string) {
	nodeOnce.Do(func() {
		nodeAddr = os.Getenv("CAESIUM_NODE_ADDRESS")
		hostName, _ = os.Hostname()
	})
	return nodeAddr, hostName
}

func readDirective(dir string) *Directive {
	raw, err := os.ReadFile(filepath.Join(dir, PauseFile))
	if err != nil {
		return nil
	}
	var d Directive
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil
	}
	if !d.Armed {
		return nil
	}
	return &d
}

func (d *Directive) matches(path, eventType, runID string) bool {
	if d == nil || !d.Armed {
		return false
	}
	if d.RunID != "" && d.RunID != runID {
		return false
	}
	if len(d.Paths) > 0 && !contains(d.Paths, path) {
		return false
	}
	if len(d.Types) > 0 && !contains(d.Types, eventType) {
		return false
	}
	return true
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func appendEntry(dir string, e entry) {
	node, host := identity()
	e.Banner = ControlBanner
	e.At = time.Now().UTC().Format(time.RFC3339Nano)
	e.PID = os.Getpid()
	e.Node = node
	e.Host = host
	raw, err := json.Marshal(e)
	if err != nil {
		return
	}
	logMu.Lock()
	defer logMu.Unlock()
	fh, err := os.OpenFile(filepath.Join(dir, LogFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = fh.Close() }()
	_, _ = fh.Write(append(raw, '\n'))
}

// BeforeBusPublish is consulted for one execution event AFTER its row is
// durably committed and immediately BEFORE its first bus.Publish, on both
// publication paths. Store marking stays after publication, so a held event's
// row remains bus_dispatch_pending and is replayed durably by a survivor.
//
// When the directive matches, the call blocks until the directive is removed
// or disarmed, the caller's context is cancelled, or max_hold_ms elapses
// (default 60s). Entry and release are both recorded in LogFile, so a test has
// independent evidence that the process really entered the hook rather than
// inferring it from a delivery gap.
func BeforeBusPublish(ctx context.Context, path, eventType, runID string, sequence uint64) {
	dir := controlDir()
	if dir == "" {
		return
	}
	d := readDirective(dir)
	if !d.matches(path, eventType, runID) {
		return
	}

	appendEntry(dir, entry{Phase: "enter", Path: path, Type: eventType, RunID: runID, Sequence: sequence})

	maxHold := defaultMaxHold
	if d.MaxHoldMS > 0 {
		maxHold = time.Duration(d.MaxHoldMS) * time.Millisecond
	}
	started := time.Now()
	deadline := started.Add(maxHold)
	ticker := time.NewTicker(holdPollPeriod)
	defer ticker.Stop()

	reason := "disarmed"
	for {
		if ctx != nil && ctx.Err() != nil {
			reason = "context_done"
			break
		}
		if time.Now().After(deadline) {
			reason = "max_hold"
			break
		}
		if !readDirective(dir).matches(path, eventType, runID) {
			reason = "disarmed"
			break
		}
		<-ticker.C
	}

	appendEntry(dir, entry{
		Phase:    "release",
		Path:     path,
		Type:     eventType,
		RunID:    runID,
		Sequence: sequence,
		HeldMS:   time.Since(started).Milliseconds(),
		Reason:   reason,
	})
}
