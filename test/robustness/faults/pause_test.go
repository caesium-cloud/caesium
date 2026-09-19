package faults

import "testing"

const (
	runningListing = "TASK                                PID     STATUS\n" +
		"abcdef1234567890deadbeefcafebabe    4711    RUNNING\n"
	pausedListing = "TASK                                PID     STATUS\n" +
		"abcdef1234567890deadbeefcafebabe    4711    PAUSED\n"
	emptyListing = "TASK                                PID     STATUS\n"
	cid          = "abcdef1234567890deadbeefcafebabe"
)

func TestTaskStatusReadsRuntimeState(t *testing.T) {
	if got, err := TaskStatus(runningListing, cid); err != nil || got != TaskRunning {
		t.Fatalf("running: got %q err %v", got, err)
	}
	if got, err := TaskStatus(pausedListing, cid); err != nil || got != TaskPaused {
		t.Fatalf("paused: got %q err %v", got, err)
	}
}

// An unreachable containerd returns an error string. Reading that as a state
// would make a broken control command look like a successful freeze.
func TestTaskStatusRejectsErrorOutput(t *testing.T) {
	if _, err := TaskStatus("ctr: failed to dial containerd: connection refused", cid); err == nil {
		t.Fatal("a ctr error was accepted as a task state")
	}
	if _, err := TaskStatus("", cid); err == nil {
		t.Fatal("empty output was accepted as a task state")
	}
}

func TestTaskStatusRejectsAbsentContainer(t *testing.T) {
	if _, err := TaskStatus(emptyListing, cid); err == nil {
		t.Fatal("an absent container was reported as a state")
	}
}

func TestTaskStatusRejectsShortID(t *testing.T) {
	if _, err := TaskStatus(runningListing, "abc"); err == nil {
		t.Fatal("a too-short id could match the wrong container and must be rejected")
	}
}

func good() PauseEvidence {
	return PauseEvidence{
		ContainerID:      cid,
		StateBefore:      TaskRunning,
		StatePaused:      TaskPaused,
		StateResumed:     TaskRunning,
		ReachableBefore:  true,
		DuringPauseErr:   "dial tcp 10.244.2.7:8080: i/o timeout",
		ReachableAfter:   true,
		HeldSeconds:      42,
		LeaseTTLSeconds:  30,
		ObservedNodeAddr: "10.244.2.7:9001",
	}
}

func TestPauseActivationNeedsTwoIndependentObservations(t *testing.T) {
	if err := good().Activated(); err != nil {
		t.Fatalf("real pause evidence rejected: %v", err)
	}

	e := good()
	e.StatePaused = TaskRunning
	if err := e.Activated(); err == nil {
		t.Fatal("runtime still RUNNING must not count as a freeze")
	}

	e = good()
	e.DuringPauseErr = ""
	if err := e.Activated(); err == nil {
		t.Fatal("a frozen member that still answers HTTP had no observable effect")
	}

	e = good()
	e.ReachableBefore = false
	if err := e.Activated(); err == nil {
		t.Fatal("an already-unreachable target cannot prove a new freeze")
	}
}

func TestPauseHealNeedsRuntimeAndService(t *testing.T) {
	if err := good().Healed(); err != nil {
		t.Fatalf("real heal evidence rejected: %v", err)
	}
	e := good()
	e.ReachableAfter = false
	if err := e.Healed(); err == nil {
		t.Fatal("heal must require the member to answer again")
	}
	e = good()
	e.StateResumed = TaskPaused
	if err := e.Healed(); err == nil {
		t.Fatal("heal must require the runtime to report RUNNING")
	}
}

func TestHeldPastLeaseIsReportedNotAsserted(t *testing.T) {
	if !good().HeldPastLease() {
		t.Fatal("a 42s freeze against a 30s lease should be reported as past the lease")
	}
	e := good()
	e.HeldSeconds = 5
	if e.HeldPastLease() {
		t.Fatal("a 5s freeze is not past a 30s lease")
	}
}
