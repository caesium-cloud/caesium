package faults

import (
	"fmt"
	"strings"
)

// Pause/resume is an EXTERNAL process freeze of a real member (A1's clock
// table: "External process STOP/CONT ... freeze an actual owner past its lease
// and resume stale work without changing the host clock"). It is a distinct
// fault from SIGKILL, disk failure and clock skew, and none of them substitutes
// for another.
//
// The state is read back from the container runtime's own task listing, so
// activation and heal are observed rather than inferred from the control
// command's exit status.

// Task states reported by `ctr tasks list`.
const (
	TaskRunning = "RUNNING"
	TaskPaused  = "PAUSED"
	TaskStopped = "STOPPED"
	TaskUnknown = ""
)

var taskListingErrorMarkers = []string{
	"failed to dial",
	"connection refused",
	"cannot connect",
	"no such file or directory",
	"permission denied",
	"i/o timeout",
	"deadline exceeded",
	"rpc error",
	"unavailable",
	"transport is closing",
	"error response from daemon",
}

// ValidTaskListing reports whether text is a real `ctr tasks list` table.
//
// An error string from ctr is evidence of nothing. Treating it as "the task is
// gone" would turn an unreachable containerd into a passing pause.
func ValidTaskListing(text string) bool {
	stripped := strings.TrimSpace(text)
	if stripped == "" {
		return false
	}
	var header string
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			header = line
			break
		}
	}
	tokens := map[string]bool{}
	for _, f := range strings.Fields(header) {
		tokens[strings.ToUpper(f)] = true
	}
	if !tokens["TASK"] || (!tokens["PID"] && !tokens["STATUS"]) {
		return false
	}
	lower := strings.ToLower(stripped)
	head := lower
	if len(head) > 120 {
		head = head[:120]
	}
	if strings.HasPrefix(lower, "ctr:") || strings.Contains(head, "error:") {
		return false
	}
	for _, marker := range taskListingErrorMarkers {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	return true
}

// TaskStatus returns the state of containerID in a `ctr tasks list` listing.
//
// It returns an error for an invalid listing or an absent container rather than
// a state, so a missing observation can never be read as a fault state.
func TaskStatus(listing, containerID string) (string, error) {
	id := strings.TrimSpace(containerID)
	if id == "" {
		return TaskUnknown, fmt.Errorf("empty container id")
	}
	short := id
	if len(short) > 12 {
		short = short[:12]
	}
	if len(short) < 8 {
		return TaskUnknown, fmt.Errorf("container id %q is too short to match", containerID)
	}
	if !ValidTaskListing(listing) {
		return TaskUnknown, fmt.Errorf("ctr task listing is not a valid table, so it proves nothing")
	}
	for _, line := range strings.Split(listing, "\n") {
		if !strings.Contains(line, id) && !strings.Contains(line, short) {
			continue
		}
		upper := strings.ToUpper(line)
		switch {
		case strings.Contains(upper, TaskPaused), strings.Contains(upper, "PAUSING"):
			return TaskPaused, nil
		case strings.Contains(upper, TaskRunning):
			return TaskRunning, nil
		case strings.Contains(upper, TaskStopped), strings.Contains(upper, "EXITED"), strings.Contains(upper, "KILLED"):
			return TaskStopped, nil
		default:
			return TaskUnknown, fmt.Errorf("container %s present with an unrecognised state: %q", short, strings.TrimSpace(line))
		}
	}
	return TaskUnknown, fmt.Errorf("container %s is absent from the task listing", short)
}

// PauseEvidence is the independent activation/heal evidence for one freeze.
type PauseEvidence struct {
	ContainerID string
	// StateBefore/StatePaused/StateResumed come from `ctr tasks list`.
	StateBefore  string
	StatePaused  string
	StateResumed string
	// ReachableBefore/DuringPause/AfterResume come from an HTTP request the
	// runner makes to the frozen member itself, outside the container runtime.
	ReachableBefore  bool
	DuringPauseErr   string
	ReachableAfter   bool
	HeldSeconds      float64
	LeaseTTLSeconds  float64
	ObservedNodeAddr string
}

// Activated returns nil when the freeze is supported by two independent
// observations: the runtime reports PAUSED, and the member stops answering.
func (e PauseEvidence) Activated() error {
	if e.StateBefore != TaskRunning {
		return fmt.Errorf("target was %q before the pause, not RUNNING", e.StateBefore)
	}
	if !e.ReachableBefore {
		return fmt.Errorf("target %s was already unreachable before the pause", e.ObservedNodeAddr)
	}
	if e.StatePaused != TaskPaused {
		return fmt.Errorf("container runtime reported %q, not PAUSED", e.StatePaused)
	}
	if e.DuringPauseErr == "" {
		return fmt.Errorf("frozen member %s still answered HTTP: the freeze had no observable effect", e.ObservedNodeAddr)
	}
	return nil
}

// Healed returns nil when resume is supported by the same two observations.
func (e PauseEvidence) Healed() error {
	if e.StateResumed != TaskRunning {
		return fmt.Errorf("container runtime reported %q after resume, not RUNNING", e.StateResumed)
	}
	if !e.ReachableAfter {
		return fmt.Errorf("member %s did not answer HTTP again after resume", e.ObservedNodeAddr)
	}
	return nil
}

// HeldPastLease reports whether the freeze outlasted the configured run lease,
// which is what makes a resumed process a stale one. It is deliberately a
// report, not an assertion: the resulting takeover behaviour is B3's scenario.
func (e PauseEvidence) HeldPastLease() bool {
	return e.LeaseTTLSeconds > 0 && e.HeldSeconds > e.LeaseTTLSeconds
}
