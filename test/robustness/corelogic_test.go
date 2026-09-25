//go:build !integration

package robustness

import (
	"strings"
	"testing"
	"time"
)

func TestStateDiffsDetectsMutationAndEquality(t *testing.T) {
	before := StateFingerprint{
		RunID: "run-1", JobID: "job-1", Status: "running",
		LeaseOwner: "10.0.0.1:9001", LeaseGeneration: 1,
		EffectNonces: []string{"n1"},
		Tasks:        []TaskFingerprint{{ID: "t1", Status: "running", Image: "img:1", Attempt: 1}},
	}
	if diffs := StateDiffs(before, before); len(diffs) != 0 {
		t.Fatalf("identical fingerprints reported diffs %v", diffs)
	}
	after := before
	after.Tasks[0].Status = "succeeded"
	after.EffectNonces = []string{"n1", "n2"}
	diffs := StateDiffs(before, after)
	if len(diffs) == 0 {
		t.Fatal("status and effect mutation was not reported")
	}
	joined := strings.Join(diffs, ",")
	if !strings.Contains(joined, "status") && !strings.Contains(joined, "effect") && !strings.Contains(joined, "tasks[0].status") {
		t.Fatalf("diffs %v missed the planted mutation", diffs)
	}
}

func TestClassifyQuorumLossNeverRejects(t *testing.T) {
	if got := ClassifyQuorumLossMutation(0, nil, errString("context deadline exceeded")); got != OutcomePossiblyCommitted {
		t.Fatalf("timeout: got %s", got)
	}
	if got := ClassifyQuorumLossMutation(500, []byte("leader unavailable"), nil); got != OutcomePossiblyCommitted {
		t.Fatalf("5xx: got %s", got)
	}
	if got := ClassifyQuorumLossMutation(409, []byte(`{"code":"conflict"}`), nil); got != OutcomePossiblyCommitted {
		t.Fatalf("409 during quorum loss is not a verified rejection: got %s", got)
	}
	if got := ClassifyQuorumLossMutation(202, []byte(`{"id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}`), nil); got != OutcomeAccepted {
		t.Fatalf("202+uuid: got %s", got)
	}
	if got := ClassifyQuorumLossMutation(202, []byte(`{}`), nil); got != OutcomePossiblyCommitted {
		t.Fatalf("bare 202 is not DT-ADMIT-01 accepted: got %s", got)
	}
}

func TestClassifyLostResponseTimeoutIsPossiblyCommitted(t *testing.T) {
	got := ClassifyLostResponse(errString("Client.Timeout"), 202, "run-uuid")
	if got != OutcomePossiblyCommitted {
		t.Fatalf("lost 202: got %s", got)
	}
	if ClassifyLostResponse(errString("timeout"), 0, "") == OutcomeRejected {
		t.Fatal("a client timeout must not be classified as rejected")
	}
}

func TestFanInRequiresWholePredecessorGroup(t *testing.T) {
	t0 := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	events := []TimedStep{
		{Step: "left", Kind: "complete", At: t0.Add(2 * time.Second)},
		{Step: "join", Kind: "start", At: t0.Add(3 * time.Second)},
	}
	if err := FanInStartedTooEarly(events, "join", []string{"left", "right"}); err == nil {
		t.Fatal("join after only one predecessor must be a defect")
	}
	events = append(events, TimedStep{Step: "right", Kind: "complete", At: t0.Add(4 * time.Second)})
	if err := FanInStartedTooEarly(events, "join", []string{"left", "right"}); err == nil {
		t.Fatal("join started before right completed must be a defect")
	}
	legal := []TimedStep{
		{Step: "left", Kind: "complete", At: t0.Add(2 * time.Second)},
		{Step: "right", Kind: "complete", At: t0.Add(4 * time.Second)},
		{Step: "join", Kind: "start", At: t0.Add(5 * time.Second)},
	}
	if err := FanInStartedTooEarly(legal, "join", []string{"left", "right"}); err != nil {
		t.Fatalf("legal fan-in: %v", err)
	}
}

func TestPromCounterAbsentIsNotARise(t *testing.T) {
	text := "# HELP x\ncaesium_dispatch_rejected_total{reason=\"no_capacity\"} 4\n"
	if v, ok := PromCounter(text, MetricDispatchRejected, map[string]string{"reason": DispatchReasonNetworkError}); ok || v != 0 {
		t.Fatalf("absent series must be (0,false), got %v %t", v, ok)
	}
	if v, ok := PromCounter(text, MetricDispatchRejected, map[string]string{"reason": "no_capacity"}); !ok || v != 4 {
		t.Fatalf("labelled counter: got %v %t", v, ok)
	}
}

func TestRedactSecretsStripsTokensAndPEM(t *testing.T) {
	in := "Authorization: Bearer super-secret-token\n-----BEGIN EC PRIVATE KEY-----\nABC\n-----END EC PRIVATE KEY-----\ncaesium-robustness-manual-key"
	out := RedactSecrets(in)
	if strings.Contains(out, "super-secret-token") || strings.Contains(out, "BEGIN EC") || strings.Contains(out, "caesium-robustness-manual-key") {
		t.Fatalf("secrets leaked: %q", out)
	}
	if !strings.Contains(out, "[redacted]") && !strings.Contains(out, "[redacted-pem]") {
		t.Fatalf("expected redaction markers: %q", out)
	}
}

func TestFrozenRecipeChanged(t *testing.T) {
	orig := TaskFingerprint{ID: "t", Image: "img:v1", Command: `["sh","-c","exit 1"]`}
	if err := FrozenRecipeChanged(orig, orig); err != nil {
		t.Fatalf("identical: %v", err)
	}
	edited := orig
	edited.Image = "img:v2"
	if err := FrozenRecipeChanged(orig, edited); err == nil {
		t.Fatal("image drift must be a defect")
	}
}

func TestParseRefusalUnauthorizedAndStaleGeneration(t *testing.T) {
	code, _ := ParseRefusal(401, []byte("unauthorized"))
	if code != RefusalUnauthorized {
		t.Fatalf("401 code=%q", code)
	}
	code, msg := ParseRefusal(409, []byte(`{"code":"stale_generation","message":"owner generation mismatch: expected 2, got 1"}`))
	if code != RefusalStaleGeneration {
		t.Fatalf("409 code=%q", code)
	}
	if !strings.Contains(msg, "generation") {
		t.Fatalf("refusal message lost: %q", msg)
	}
}

func TestTerminalCompleteRefusalAllowed(t *testing.T) {
	if !TerminalCompleteRefusalAllowed(409, RefusalTerminalRun) ||
		!TerminalCompleteRefusalAllowed(409, RefusalWrongWorker) {
		t.Fatal("claim/terminal 409 must be allowed")
	}
	if TerminalCompleteRefusalAllowed(409, RefusalNotOwner) ||
		TerminalCompleteRefusalAllowed(409, RefusalMissingRun) ||
		TerminalCompleteRefusalAllowed(409, RefusalTaskNotRunning) ||
		TerminalCompleteRefusalAllowed(409, RefusalCompletionRejected) ||
		TerminalCompleteRefusalAllowed(503, RefusalTaskNotRunning) ||
		TerminalCompleteRefusalAllowed(200, RefusalTaskNotRunning) {
		t.Fatal("not_owner, missing_run, 5xx and 200 must not count as the terminal fence")
	}
}

func TestCancelledCompleteRefusalAllowed(t *testing.T) {
	for _, code := range []string{
		RefusalTerminalRun, RefusalWrongWorker,
		RefusalNotOwner, RefusalMissingRun, RefusalStaleGeneration,
	} {
		if !CancelledCompleteRefusalAllowed(409, code) {
			t.Fatalf("409 %s must fence an old completion", code)
		}
	}
	for _, tc := range []struct {
		status int
		code   string
	}{{200, ""}, {503, "owner_not_ready"}, {409, ""},
		{409, RefusalCompletionRejected}, {409, RefusalTaskNotRunning}} {
		if CancelledCompleteRefusalAllowed(tc.status, tc.code) {
			t.Fatalf("%d %s must not prove a cancellation fence", tc.status, tc.code)
		}
	}
}

func TestIsTLSHandshakeAlert(t *testing.T) {
	if !IsTLSHandshakeAlert("remote error: tls: bad certificate") {
		t.Fatal("server alert must match")
	}
	if !IsTLSHandshakeAlert("tls: certificate required") {
		t.Fatal("certificate required must match")
	}
	for _, n := range []string{"dial tcp i/o timeout", "connection refused", "401 unauthorized", ""} {
		if IsTLSHandshakeAlert(n) {
			t.Fatalf("%q must not count as a TLS alert", n)
		}
	}
}

func TestMaxBenchedNetworkErrors(t *testing.T) {
	if got := MaxBenchedNetworkErrors(20*time.Second, 10*time.Second); got != 3 {
		t.Fatalf("20s/10s: got %d want 3", got)
	}
	if got := MaxBenchedNetworkErrors(5*time.Second, 10*time.Second); got != 1 {
		t.Fatalf("window shorter than cooldown: got %d want 1", got)
	}
	if MaxBenchedNetworkErrors(20*time.Second, 10*time.Second) >= 8 {
		t.Fatal("the cap must be well below an unbenched 1s-tick burst")
	}
}

func TestSplitDropActive(t *testing.T) {
	if SplitDropActive(0, 0) {
		t.Fatal("zero counters are not activation")
	}
	if !SplitDropActive(3, 0) || !SplitDropActive(0, 2) {
		t.Fatal("raft or internal packets must activate the drop")
	}
}

func TestLoadCoreFixtureYAML(t *testing.T) {
	for _, name := range []string{"fan-in.job.yaml", "fail-then-retry.job.yaml", "replace-concurrency.job.yaml"} {
		def, err := LoadCoreFixture(name, "alias-"+name[:4], "example.net/task:test")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if def.Metadata.Alias == "" || len(def.Steps) == 0 {
			t.Fatalf("%s parsed empty", name)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
