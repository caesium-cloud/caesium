package faults

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The durable-event-before-publication boundary is the only product
// instrumentation this plan allows, and its evidence must be produced by the
// server process itself, not inferred. Each consultation appends an `enter`
// record and, when it releases, a `release` record carrying the publication
// path, the event identity and how long it was held. A pause that produced no
// `enter` record did not happen, whatever the delivery history looks like.

// HookPathDispatchOnce and HookPathPublishAndMark mirror the product-side
// constants. They are repeated here so the test asserts against the published
// contract rather than importing the instrumented package into the runner.
const (
	HookPathDispatchOnce   = "dispatch_once"
	HookPathPublishAndMark = "publish_and_mark"
	HookBanner             = "caesium-testfault-control"
)

// HookEntry is one record from the instrumented server's hook log.
type HookEntry struct {
	Banner   string `json:"banner"`
	At       string `json:"at"`
	Phase    string `json:"phase"`
	Path     string `json:"path"`
	Type     string `json:"type"`
	RunID    string `json:"run_id"`
	Sequence uint64 `json:"sequence"`
	PID      int    `json:"pid"`
	Node     string `json:"node"`
	Host     string `json:"host"`
	HeldMS   int64  `json:"held_ms"`
	Reason   string `json:"reason"`
}

// ParseHookLog reads the append-only JSONL hook record.
//
// An unreadable line is an error rather than a skipped record: a truncated or
// corrupt log makes the hook evidence inconclusive, and inconclusive must never
// silently become "no pause happened".
func ParseHookLog(text string) ([]HookEntry, error) {
	var out []HookEntry
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e HookEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return out, fmt.Errorf("hook log line %d is not JSON: %w", i+1, err)
		}
		if e.Banner != HookBanner {
			return out, fmt.Errorf("hook log line %d has banner %q, want %q", i+1, e.Banner, HookBanner)
		}
		out = append(out, e)
	}
	return out, nil
}

// HookEvidence summarises one member's hook log for one run.
type HookEvidence struct {
	Member   string
	Entered  []HookEntry
	Released []HookEntry
	Paths    map[string]int
}

// SummariseHookLog filters the log to one run.
func SummariseHookLog(member, runID string, entries []HookEntry) HookEvidence {
	ev := HookEvidence{Member: member, Paths: map[string]int{}}
	for _, e := range entries {
		if runID != "" && e.RunID != runID {
			continue
		}
		switch e.Phase {
		case "enter":
			ev.Entered = append(ev.Entered, e)
			ev.Paths[e.Path]++
		case "release":
			ev.Released = append(ev.Released, e)
		}
	}
	return ev
}

// Activated returns nil when the server really entered the hook for this run on
// a recognised publication path.
func (e HookEvidence) Activated() error {
	if len(e.Entered) == 0 {
		return fmt.Errorf("member %s never entered the publication hook for the selected run", e.Member)
	}
	for _, entry := range e.Entered {
		if entry.Path != HookPathDispatchOnce && entry.Path != HookPathPublishAndMark {
			return fmt.Errorf("member %s recorded an unknown publication path %q", e.Member, entry.Path)
		}
		if entry.Sequence == 0 {
			return fmt.Errorf("member %s held an event with no sequence, so the committed row cannot be identified", e.Member)
		}
	}
	return nil
}

// ReleasedByDisarm returns nil when every hold ended because the test disarmed
// it. A hold that ended at max_hold or on a cancelled context means the fault
// expired on its own, so any delivery seen afterwards proves nothing about the
// pause.
func (e HookEvidence) ReleasedByDisarm() error {
	if len(e.Released) == 0 {
		return fmt.Errorf("member %s never released the hook", e.Member)
	}
	for _, entry := range e.Released {
		if entry.Reason != "disarmed" {
			return fmt.Errorf("member %s released the hook because of %q, not the test disarming it", e.Member, entry.Reason)
		}
	}
	return nil
}

// ReconcileReleases requires that every hold recorded at ACTIVATION time later
// has a matching `disarmed` release on the same member.
//
// The weaker check this replaces walked the current logs and skipped any member
// whose summary was empty, so a log that was truncated, emptied or lost between
// the two reads was indistinguishable from a member that never entered the
// hook — and the hold it was hiding would have gone unproven. Activation
// already recorded which members, paths and sequences entered; that set is the
// obligation, and anything that cannot be reconciled against it is an error,
// never a skip.
func ReconcileReleases(activation, current []HookEvidence) error {
	byMember := map[string]HookEvidence{}
	for _, ev := range current {
		byMember[ev.Member] = ev
	}
	for _, act := range activation {
		if len(act.Entered) == 0 {
			continue
		}
		cur, ok := byMember[act.Member]
		if !ok {
			return fmt.Errorf("member %s recorded %d hook entries at activation but has no hook evidence now: the record disappeared",
				act.Member, len(act.Entered))
		}
		entered := map[uint64]struct{}{}
		for _, seq := range cur.HeldSequences() {
			entered[seq] = struct{}{}
		}
		released := map[uint64]string{}
		for _, rel := range cur.Released {
			if rel.Sequence == 0 {
				return fmt.Errorf("member %s released a hold with no sequence, so it cannot be matched to an entry", act.Member)
			}
			released[rel.Sequence] = rel.Reason
		}
		for _, seq := range act.HeldSequences() {
			if _, ok := entered[seq]; !ok {
				return fmt.Errorf("member %s no longer records entering the hook for sequence %d: the hook log lost evidence it had already produced",
					act.Member, seq)
			}
			reason, ok := released[seq]
			if !ok {
				return fmt.Errorf("member %s never released its hold on sequence %d", act.Member, seq)
			}
			if reason != "disarmed" {
				return fmt.Errorf("member %s released sequence %d because of %q, not the test disarming it",
					act.Member, seq, reason)
			}
		}
	}
	return nil
}

// HeldSequences returns the event sequences this member held.
func (e HookEvidence) HeldSequences() []uint64 {
	seen := map[uint64]struct{}{}
	var out []uint64
	for _, entry := range e.Entered {
		if entry.Sequence == 0 {
			continue
		}
		if _, ok := seen[entry.Sequence]; ok {
			continue
		}
		seen[entry.Sequence] = struct{}{}
		out = append(out, entry.Sequence)
	}
	return out
}
