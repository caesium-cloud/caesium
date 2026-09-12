//go:build !integration

package robustness

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/test/robustness/recorder"
)

func TestCheckSuccessorOrdering(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 15, 0, 0, 0, time.UTC)
	runID := "run-1"
	events := []recorder.Event{
		{Kind: "start", At: t0, RunID: runID, Step: "block"},
		{Kind: "release", At: t0.Add(2 * time.Second), RunID: runID},
		{Kind: "complete", At: t0.Add(3 * time.Second), RunID: runID, Step: "block"},
		{Kind: "start", At: t0.Add(4 * time.Second), RunID: runID, Step: "successor"},
		{Kind: "complete", At: t0.Add(5 * time.Second), RunID: runID, Step: "successor"},
	}
	if err := checkSuccessorOrdering(events, runID, "block", "successor"); err != nil {
		t.Fatalf("legal order: %v", err)
	}

	early := append([]recorder.Event{}, events...)
	early[3].At = t0.Add(2500 * time.Millisecond)
	if err := checkSuccessorOrdering(early, runID, "block", "successor"); err == nil {
		t.Fatalf("successor after release but before block complete must fail")
	}

	held := append([]recorder.Event{}, events...)
	held[3].At = t0.Add(time.Second)
	if err := checkSuccessorOrdering(held, runID, "block", "successor"); err == nil {
		t.Fatalf("successor while block held must fail")
	}

	dup := append([]recorder.Event{}, events...)
	dup = append(dup, recorder.Event{Kind: "start", At: t0.Add(6 * time.Second), RunID: runID, Step: "block"})
	if err := checkSuccessorOrdering(dup, runID, "block", "successor"); err != nil {
		t.Fatalf("duplicate block attempt after successor is allowed: %v", err)
	}
}

func TestCheckPublicTerminalAgreement(t *testing.T) {
	tasks := []publicTask{
		{Step: "block", Status: "succeeded"},
		{Step: "block", Status: "cancelled"},
		{Step: "successor", Status: "succeeded"},
	}
	if err := checkPublicTerminalAgreement(tasks, "block", "successor"); err != nil {
		t.Fatalf("duplicate cancelled attempt should be allowed: %v", err)
	}
	if err := checkPublicTerminalAgreement([]publicTask{
		{Step: "block", Status: "succeeded"},
		{Step: "successor", Status: "running"},
	}, "block", "successor"); err == nil {
		t.Fatalf("non-terminal successor must fail")
	}
	if err := checkPublicTerminalAgreement([]publicTask{
		{Step: "block", Status: "failed"},
		{Step: "successor", Status: "succeeded"},
	}, "block", "successor"); err == nil {
		t.Fatalf("missing successful block must fail")
	}
}
