package robustness

import (
	"fmt"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/test/robustness/recorder"
)

type publicTask struct {
	Step   string
	Status string
}

func earliestEvent(events []recorder.Event) (recorder.Event, bool) {
	if len(events) == 0 {
		return recorder.Event{}, false
	}
	min := events[0]
	for _, ev := range events[1:] {
		if ev.At.IsZero() {
			continue
		}
		if min.At.IsZero() || ev.At.Before(min.At) {
			min = ev
		}
	}
	if min.At.IsZero() {
		return recorder.Event{}, false
	}
	return min, true
}

func checkSuccessorOrdering(events []recorder.Event, runID, blockStep, succStep string) error {
	var release, blockStarts, blockDone, succStarts, succDone []recorder.Event
	for _, ev := range events {
		if ev.RunID != runID {
			continue
		}
		if ev.At.IsZero() {
			return fmt.Errorf("recorder %s event for %s missing timestamp", ev.Kind, ev.Step)
		}
		switch ev.Kind {
		case "release":
			release = append(release, ev)
		case "start":
			switch ev.Step {
			case blockStep:
				blockStarts = append(blockStarts, ev)
			case succStep:
				succStarts = append(succStarts, ev)
			}
		case "complete":
			switch ev.Step {
			case blockStep:
				blockDone = append(blockDone, ev)
			case succStep:
				succDone = append(succDone, ev)
			}
		}
	}
	if len(blockStarts) == 0 || len(blockDone) == 0 {
		return fmt.Errorf("recorder missing block start/complete: starts=%d completes=%d", len(blockStarts), len(blockDone))
	}
	if len(succStarts) == 0 || len(succDone) == 0 {
		return fmt.Errorf("legal successor did not execute: starts=%d completes=%d", len(succStarts), len(succDone))
	}
	firstBlockDone, ok := earliestEvent(blockDone)
	if !ok {
		return fmt.Errorf("block completion missing timestamp")
	}
	firstSucc, ok := earliestEvent(succStarts)
	if !ok {
		return fmt.Errorf("successor start missing timestamp")
	}
	if firstSucc.At.Before(firstBlockDone.At) {
		return fmt.Errorf("successor started at %s before block completed at %s", firstSucc.At.UTC().Format(time.RFC3339Nano), firstBlockDone.At.UTC().Format(time.RFC3339Nano))
	}
	firstRel, ok := earliestEvent(release)
	if !ok {
		return fmt.Errorf("recorder missing release event while block was held")
	}
	if firstSucc.At.Before(firstRel.At) {
		return fmt.Errorf("successor started at %s while block was held (release at %s)", firstSucc.At.UTC().Format(time.RFC3339Nano), firstRel.At.UTC().Format(time.RFC3339Nano))
	}
	for _, ev := range succStarts {
		if ev.At.Before(firstBlockDone.At) {
			return fmt.Errorf("successor start %s precedes block completion %s", ev.At.UTC().Format(time.RFC3339Nano), firstBlockDone.At.UTC().Format(time.RFC3339Nano))
		}
	}
	return nil
}

func isPublicTerminal(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "succeeded", "failed", "skipped", "cached", "cancelled":
		return true
	default:
		return false
	}
}

func isPublicSuccess(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "succeeded", "cached":
		return true
	default:
		return false
	}
}

func checkPublicTerminalAgreement(tasks []publicTask, blockStep, succStep string) error {
	blockN, succN := 0, 0
	blockOK, succOK := false, false
	for _, t := range tasks {
		switch t.Step {
		case blockStep:
			blockN++
			if !isPublicTerminal(t.Status) {
				return fmt.Errorf("public %s task status %q is not terminal", blockStep, t.Status)
			}
			if isPublicSuccess(t.Status) {
				blockOK = true
			}
		case succStep:
			succN++
			if !isPublicTerminal(t.Status) {
				return fmt.Errorf("public %s task status %q is not terminal", succStep, t.Status)
			}
			if isPublicSuccess(t.Status) {
				succOK = true
			}
		}
	}
	if blockN == 0 || succN == 0 {
		return fmt.Errorf("public tasks missing required steps block=%d successor=%d", blockN, succN)
	}
	if !blockOK || !succOK {
		return fmt.Errorf("public terminal states do not agree with completed effects (block_success=%t successor_success=%t)", blockOK, succOK)
	}
	return nil
}
