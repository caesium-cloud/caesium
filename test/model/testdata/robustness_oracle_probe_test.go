// This file is copied into test/robustness in an isolated checkout by
// scripts/validate-test-oracles.sh. It drives the actual B3 request and
// ordering classifiers without starting a cluster.
package robustness

import (
	"fmt"
	"testing"
	"time"
)

func TestC3FanInStartedTooEarly(t *testing.T) {
	base := time.Unix(100, 0)
	events := []TimedStep{
		{Step: "left-0", Kind: "complete", At: base},
		{Step: "join", Kind: "start", At: base.Add(time.Second)},
		{Step: "left-1", Kind: "complete", At: base.Add(2 * time.Second)},
	}
	if err := FanInStartedTooEarly(events, "join", []string{"left-0", "left-1"}); err == nil {
		t.Fatal("join started before all predecessor partitions completed")
	}
	events[1].At = base.Add(3 * time.Second)
	if err := FanInStartedTooEarly(events, "join", []string{"left-0", "left-1"}); err != nil {
		t.Fatalf("legal fan-in order rejected: %v", err)
	}
}

func TestC3AmbiguousTimeoutHistory(t *testing.T) {
	// The controller might have committed before a response was lost. Neither
	// a transport timeout nor a 5xx during quorum loss proves rejection.
	if got := ClassifyLostResponse(fmt.Errorf("client timeout"), 202, "acknowledged-run"); got != OutcomePossiblyCommitted {
		t.Fatalf("lost response was classified %s", got)
	}
	if got := ClassifyQuorumLossMutation(0, nil, fmt.Errorf("client timeout")); got != OutcomePossiblyCommitted {
		t.Fatalf("quorum-loss timeout was classified %s", got)
	}
	if got := ClassifyQuorumLossMutation(500, []byte(`{"error":"unavailable"}`), nil); got != OutcomePossiblyCommitted {
		t.Fatalf("quorum-loss 500 was classified %s", got)
	}
	if got := ClassifyQuorumLossMutation(202, []byte(`{"id":"acknowledged-run"}`), nil); got != OutcomeAccepted {
		t.Fatalf("UUID-bearing acknowledgement was classified %s", got)
	}
}
