package faults

import (
	"strings"
	"testing"
)

const hookLog = `{"banner":"caesium-testfault-control","at":"2026-09-16T10:00:00Z","phase":"enter","path":"dispatch_once","type":"task_started","run_id":"r1","sequence":41,"pid":7,"node":"10.244.1.5:9001"}
{"banner":"caesium-testfault-control","at":"2026-09-16T10:00:09Z","phase":"release","path":"dispatch_once","type":"task_started","run_id":"r1","sequence":41,"pid":7,"held_ms":9000,"reason":"disarmed"}
`

func TestParseHookLog(t *testing.T) {
	entries, err := ParseHookLog(hookLog)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 records, got %d", len(entries))
	}
	if entries[0].Sequence != 41 || entries[0].Path != HookPathDispatchOnce {
		t.Fatalf("hook entry misparsed: %+v", entries[0])
	}
}

// A corrupt or truncated log must be an error. Treating it as an empty log
// would turn missing evidence into "the pause never happened".
func TestParseHookLogRejectsCorruptLines(t *testing.T) {
	if _, err := ParseHookLog(hookLog + "{not json\n"); err == nil {
		t.Fatal("a corrupt hook log line was accepted")
	}
	if _, err := ParseHookLog(`{"phase":"enter","path":"dispatch_once"}`); err == nil {
		t.Fatal("a record without the control banner was accepted")
	}
}

func TestHookEvidenceActivation(t *testing.T) {
	entries, err := ParseHookLog(hookLog)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ev := SummariseHookLog("caesium-0", "r1", entries)
	if err := ev.Activated(); err != nil {
		t.Fatalf("real hook evidence rejected: %v", err)
	}
	if err := ev.ReleasedByDisarm(); err != nil {
		t.Fatalf("disarm release rejected: %v", err)
	}
	if got := ev.HeldSequences(); len(got) != 1 || got[0] != 41 {
		t.Fatalf("held sequences wrong: %v", got)
	}
	if ev.Paths[HookPathDispatchOnce] != 1 {
		t.Fatalf("publication path not counted: %v", ev.Paths)
	}
}

func TestHookEvidenceRejectsAnotherRun(t *testing.T) {
	entries, _ := ParseHookLog(hookLog)
	ev := SummariseHookLog("caesium-0", "some-other-run", entries)
	if err := ev.Activated(); err == nil {
		t.Fatal("hook records for a different run were accepted as evidence")
	}
}

// A hold that expired on its own proves nothing about what the test did next.
func TestHookEvidenceRejectsSelfExpiredHold(t *testing.T) {
	expired := strings.Replace(hookLog, `"reason":"disarmed"`, `"reason":"max_hold"`, 1)
	entries, err := ParseHookLog(expired)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ev := SummariseHookLog("caesium-0", "r1", entries)
	if err := ev.ReleasedByDisarm(); err == nil {
		t.Fatal("a max_hold expiry was accepted as a test-controlled release")
	}
}

func TestHookEvidenceRejectsUnidentifiedEvent(t *testing.T) {
	noSeq := strings.Replace(hookLog, `"sequence":41`, `"sequence":0`, 1)
	entries, _ := ParseHookLog(noSeq)
	ev := SummariseHookLog("caesium-0", "r1", entries)
	if err := ev.Activated(); err == nil {
		t.Fatal("a held event with no sequence cannot identify a committed row")
	}
}

func TestEmptyHookLogIsNotActivation(t *testing.T) {
	entries, err := ParseHookLog("")
	if err != nil || len(entries) != 0 {
		t.Fatalf("empty log: %v %v", entries, err)
	}
	if err := SummariseHookLog("caesium-0", "r1", entries).Activated(); err == nil {
		t.Fatal("an empty hook log was accepted as an activated pause")
	}
}

// enteredEv builds the activation-time evidence for one member holding one event.
func enteredEv(member string, seq uint64) HookEvidence {
	return HookEvidence{
		Member:  member,
		Entered: []HookEntry{{Banner: HookBanner, Phase: "enter", Path: HookPathDispatchOnce, Sequence: seq, RunID: "r"}},
		Paths:   map[string]int{HookPathDispatchOnce: 1},
	}
}

func releasedEv(member string, seq uint64, reason string) HookEvidence {
	ev := enteredEv(member, seq)
	ev.Released = []HookEntry{{Banner: HookBanner, Phase: "release", Path: HookPathDispatchOnce, Sequence: seq, RunID: "r", Reason: reason}}
	return ev
}

func TestReconcileReleasesAcceptsADisarmedReleaseForEveryHold(t *testing.T) {
	act := []HookEvidence{enteredEv("caesium-0", 40), enteredEv("caesium-1", 40)}
	cur := []HookEvidence{releasedEv("caesium-0", 40, "disarmed"), releasedEv("caesium-1", 40, "disarmed")}
	if err := ReconcileReleases(act, cur); err != nil {
		t.Fatalf("a fully reconciled release set was rejected: %v", err)
	}
}

// The P2 this closes: the old check walked the CURRENT logs and skipped any
// member whose summary was empty, so a log that vanished between the two reads
// passed as "this member never entered the hook".
func TestReconcileReleasesRejectsAVanishedHookLog(t *testing.T) {
	act := []HookEvidence{enteredEv("caesium-0", 40), enteredEv("caesium-1", 40)}
	cur := []HookEvidence{releasedEv("caesium-0", 40, "disarmed"), {Member: "caesium-1", Paths: map[string]int{}}}
	err := ReconcileReleases(act, cur)
	if err == nil {
		t.Fatal("a member whose recorded entry disappeared must not reconcile")
	}
	if !strings.Contains(err.Error(), "caesium-1") {
		t.Fatalf("the error must name the member that lost its evidence: %v", err)
	}
}

func TestReconcileReleasesRejectsAMissingMember(t *testing.T) {
	if err := ReconcileReleases([]HookEvidence{enteredEv("caesium-2", 7)}, nil); err == nil {
		t.Fatal("a member with no current evidence at all must not reconcile")
	}
}

func TestReconcileReleasesRejectsAnUnreleasedHold(t *testing.T) {
	act := []HookEvidence{enteredEv("caesium-0", 40)}
	if err := ReconcileReleases(act, []HookEvidence{enteredEv("caesium-0", 40)}); err == nil {
		t.Fatal("an entered hold with no release record must not reconcile")
	}
}

func TestReconcileReleasesRejectsAnExpiredHold(t *testing.T) {
	act := []HookEvidence{enteredEv("caesium-0", 40)}
	err := ReconcileReleases(act, []HookEvidence{releasedEv("caesium-0", 40, "max_hold")})
	if err == nil {
		t.Fatal("a hold that expired on its own must not read as a test-controlled release")
	}
	if !strings.Contains(err.Error(), "max_hold") {
		t.Fatalf("the error must carry the real release reason: %v", err)
	}
}

// A release for a DIFFERENT sequence cannot stand in for the one that was held.
func TestReconcileReleasesRejectsAnUnmatchedSequence(t *testing.T) {
	act := []HookEvidence{enteredEv("caesium-0", 40)}
	if err := ReconcileReleases(act, []HookEvidence{releasedEv("caesium-0", 41, "disarmed")}); err == nil {
		t.Fatal("a release of another sequence must not reconcile the recorded hold")
	}
}

func twoPathEntered(member string, seq uint64) HookEvidence {
	return HookEvidence{
		Member: member,
		Entered: []HookEntry{
			{Banner: HookBanner, Phase: "enter", Path: HookPathDispatchOnce, Sequence: seq, RunID: "r"},
			{Banner: HookBanner, Phase: "enter", Path: HookPathPublishAndMark, Sequence: seq, RunID: "r"},
		},
		Paths: map[string]int{HookPathDispatchOnce: 1, HookPathPublishAndMark: 1},
	}
}

func releaseOn(path string, seq uint64, reason string) HookEntry {
	return HookEntry{Banner: HookBanner, Phase: "release", Path: path, Sequence: seq, RunID: "r", Reason: reason}
}

// The P2 this closes: both publication paths hold the same sequence on one
// member. Releasing only one of them used to satisfy both obligations.
func TestReconcileReleasesRejectsAMissingPathOnTheSameSequence(t *testing.T) {
	act := []HookEvidence{twoPathEntered("caesium-0", 40)}
	cur := twoPathEntered("caesium-0", 40)
	cur.Released = []HookEntry{releaseOn(HookPathDispatchOnce, 40, "disarmed")}
	err := ReconcileReleases(act, []HookEvidence{cur})
	if err == nil {
		t.Fatal("releasing only one of two paths holding the same sequence must not reconcile")
	}
	if !strings.Contains(err.Error(), HookPathPublishAndMark) {
		t.Fatalf("the error must name the path that was not released: %v", err)
	}
}

// A max_hold followed by a later disarmed on the SAME hold used to pass
// because the sequence-keyed map kept only the last reason.
func TestReconcileReleasesRejectsMaxHoldThenDisarmedOnTheSameHold(t *testing.T) {
	act := []HookEvidence{enteredEv("caesium-0", 40)}
	cur := enteredEv("caesium-0", 40)
	cur.Released = []HookEntry{
		releaseOn(HookPathDispatchOnce, 40, "max_hold"),
		releaseOn(HookPathDispatchOnce, 40, "disarmed"),
	}
	err := ReconcileReleases(act, []HookEvidence{cur})
	if err == nil {
		t.Fatal("a max_hold followed by a later disarmed on the same hold must not reconcile")
	}
	if !strings.Contains(err.Error(), "max_hold") {
		t.Fatalf("the error must carry the real release reason, not the later disarmed: %v", err)
	}
}

func TestReconcileReleasesAcceptsBothPathsDisarmedOnTheSameSequence(t *testing.T) {
	act := []HookEvidence{twoPathEntered("caesium-0", 40)}
	cur := twoPathEntered("caesium-0", 40)
	cur.Released = []HookEntry{
		releaseOn(HookPathDispatchOnce, 40, "disarmed"),
		releaseOn(HookPathPublishAndMark, 40, "disarmed"),
	}
	if err := ReconcileReleases(act, []HookEvidence{cur}); err != nil {
		t.Fatalf("both paths disarmed on the same sequence must reconcile: %v", err)
	}
}

// Two enters of the same path and sequence are two obligations.
func TestReconcileReleasesPreservesMultiplicityOfTheSamePathAndSequence(t *testing.T) {
	enter := HookEntry{Banner: HookBanner, Phase: "enter", Path: HookPathDispatchOnce, Sequence: 40, RunID: "r"}
	act := []HookEvidence{{
		Member:  "caesium-0",
		Entered: []HookEntry{enter, enter},
		Paths:   map[string]int{HookPathDispatchOnce: 2},
	}}
	one := []HookEvidence{{
		Member:   "caesium-0",
		Entered:  []HookEntry{enter, enter},
		Released: []HookEntry{releaseOn(HookPathDispatchOnce, 40, "disarmed")},
		Paths:    map[string]int{HookPathDispatchOnce: 2},
	}}
	if err := ReconcileReleases(act, one); err == nil {
		t.Fatal("one release must not satisfy two enters of the same path and sequence")
	}
	two := one
	two[0].Released = append(append([]HookEntry{}, one[0].Released...), releaseOn(HookPathDispatchOnce, 40, "disarmed"))
	if err := ReconcileReleases(act, two); err != nil {
		t.Fatalf("two disarmed releases must satisfy two enters: %v", err)
	}
}
