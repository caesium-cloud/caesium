package history

import "testing"

func scope(min, max uint64) Scope {
	return Scope{Store: "test", Min: min, Max: max, Complete: true}
}

// fixtureSteps is the catalog-task-id -> step-name map the live suite reads
// from GET /v1/jobs/:id/tasks.
var fixtureSteps = map[string]string{"t1": "block", "t2": "successor"}

// Duplicates and undelivered persisted rows are both legal under DT-EVENT-01:
// delivery is at-least-once and an in-process subscriber buffer can overflow.
// Neither may be reported as a defect.
func TestDuplicateAndMissingDeliveryAreNotDefects(t *testing.T) {
	persisted := []Persisted{{Sequence: 1, Type: "run_started"}, {Sequence: 5, Type: "task_started"}, {Sequence: 9, Type: "run_completed"}}
	delivered := []Delivered{{Sequence: 1, Type: "run_started"}, {Sequence: 1, Type: "run_started"}, {Sequence: 9, Type: "run_completed"}}

	rep := Compare(delivered, persisted, scope(1, 9), open())
	if got := rep.Defects(); len(got) != 0 {
		t.Fatalf("legal history reported defects: %v", got)
	}
	if rep.Duplicates[1] != 2 {
		t.Fatalf("duplicate delivery not retained: %v", rep.Duplicates)
	}
	if len(rep.MissingFromDelivery) != 1 || rep.MissingFromDelivery[0] != 5 {
		t.Fatalf("missing delivery not recorded: %v", rep.MissingFromDelivery)
	}
	if !rep.Conclusive() {
		t.Fatalf("complete scope should be conclusive: %v", rep.Inconclusive)
	}
}

// Negative control: a delivery with no durable row in the same scope means the
// system published something it had not committed. The checker must catch it.
func TestDeliveredWithoutPersistedRowIsADefect(t *testing.T) {
	persisted := []Persisted{{Sequence: 1, Type: "run_started"}}
	delivered := []Delivered{{Sequence: 1, Type: "run_started"}, {Sequence: 7, Type: "task_started"}}

	rep := Compare(delivered, persisted, scope(1, 9), open())
	if len(rep.DeliveredNotPersisted) != 1 || rep.DeliveredNotPersisted[0] != 7 {
		t.Fatalf("planted uncommitted delivery not detected: %+v", rep)
	}
	if len(rep.Defects()) == 0 {
		t.Fatal("planted uncommitted delivery did not produce a defect")
	}
}

// Per-run sequences are sparse by construction. A checker that treated the
// holes between 3, 17 and 250 as lost events would reject every legal history.
func TestSparseSequencesAreNotGaps(t *testing.T) {
	persisted := []Persisted{{Sequence: 3, Type: "run_started"}, {Sequence: 17, Type: "task_started"}, {Sequence: 250, Type: "run_completed"}}
	delivered := []Delivered{{Sequence: 3, Type: "run_started"}, {Sequence: 17, Type: "task_started"}, {Sequence: 250, Type: "run_completed"}}

	rep := Compare(delivered, persisted, scope(3, 250), open())
	if len(rep.Defects()) != 0 {
		t.Fatalf("sparse per-run sequences reported as defects: %v", rep.Defects())
	}
	if !rep.Conclusive() {
		t.Fatalf("sparse history reported inconclusive: %v", rep.Inconclusive)
	}
	if len(rep.MissingFromDelivery) != 0 {
		t.Fatalf("sparse holes counted as missing deliveries: %v", rep.MissingFromDelivery)
	}
}

func TestIncompleteScopeIsInconclusive(t *testing.T) {
	s := scope(1, 9)
	s.Complete = false
	rep := Compare(
		[]Delivered{{Sequence: 1, Type: "run_started"}},
		[]Persisted{{Sequence: 1, Type: "run_started"}},
		s,
		open(),
	)
	if rep.Conclusive() {
		t.Fatal("truncated persisted read reported as conclusive")
	}
	if len(rep.Defects()) != 0 {
		t.Fatalf("inconclusive must not also be a defect: %v", rep.Defects())
	}
}

func TestEmptyPersistedReadIsInconclusiveNotAPass(t *testing.T) {
	rep := Compare(nil, nil, scope(0, 0), open())
	if rep.Conclusive() {
		t.Fatal("an empty persisted read must never be conclusive")
	}
}

// Live bus delivery can arrive out of sequence order when a publisher is
// delayed. That is documented behaviour, not a failure.
func TestOutOfOrderDeliveryIsLegal(t *testing.T) {
	persisted := []Persisted{{Sequence: 4, Type: "a"}, {Sequence: 6, Type: "b"}}
	delivered := []Delivered{{Sequence: 6, Type: "b"}, {Sequence: 4, Type: "a"}}
	rep := Compare(delivered, persisted, scope(4, 6), open())
	if rep.OutOfOrder != 1 {
		t.Fatalf("out-of-order delivery not recorded: %+v", rep)
	}
	if len(rep.Defects()) != 0 {
		t.Fatalf("out-of-order delivery reported as a defect: %v", rep.Defects())
	}
}

func TestTypeMismatchIsADefect(t *testing.T) {
	rep := Compare(
		[]Delivered{{Sequence: 2, Type: "run_completed"}},
		[]Persisted{{Sequence: 2, Type: "run_failed"}},
		scope(1, 5),
		open(),
	)
	if len(rep.TypeMismatch) != 1 {
		t.Fatalf("type disagreement not detected: %+v", rep)
	}
	if len(rep.Defects()) == 0 {
		t.Fatal("type disagreement did not produce a defect")
	}
}

func TestUnidentifiedDeliveryIsInconclusive(t *testing.T) {
	rep := Compare(
		[]Delivered{{Sequence: 0, Type: "run_started"}},
		[]Persisted{{Sequence: 1, Type: "run_started"}},
		scope(1, 1),
		open(),
	)
	if rep.Unidentified != 1 {
		t.Fatalf("delivery without an id not counted: %+v", rep)
	}
	if rep.Conclusive() {
		t.Fatal("a delivery with no id must make the comparison inconclusive")
	}
}

func TestOutOfScopeDeliveryIsNotADefect(t *testing.T) {
	rep := Compare(
		[]Delivered{{Sequence: 99, Type: "x"}},
		[]Persisted{{Sequence: 1, Type: "y"}},
		scope(1, 10),
		open(),
	)
	if len(rep.OutOfScope) != 1 {
		t.Fatalf("out-of-scope delivery not recorded: %+v", rep)
	}
	if len(rep.Defects()) != 0 {
		t.Fatalf("out-of-scope delivery treated as a defect: %v", rep.Defects())
	}
	if rep.Conclusive() {
		t.Fatal("out-of-scope delivery must be inconclusive, not conclusive")
	}
}

// The P2 this closes: the recorder delivered one fixture event and then broke.
// The delivered set is non-empty, every persisted row it never saw looks like
// legal at-least-once loss, and only the connection tells the two apart.
func TestBrokenSubscriptionIsInconclusiveNotLegalLoss(t *testing.T) {
	persisted := []Persisted{{Sequence: 1, Type: "run_started"}, {Sequence: 2, Type: "task_started"}, {Sequence: 3, Type: "run_succeeded"}}
	delivered := []Delivered{{Sequence: 1, Type: "run_started"}}

	healthy := Compare(delivered, persisted, scope(1, 3), open())
	if !healthy.Conclusive() {
		t.Fatalf("a live delivery gap on a healthy stream is legal: %v", healthy.Inconclusive)
	}

	broken := Compare(delivered, persisted, scope(1, 3),
		Connection{Established: true, Status: 200, Err: "unexpected EOF"})
	if broken.Conclusive() {
		t.Fatalf("a stream that broke mid-window must be inconclusive: %+v", broken)
	}
	if len(broken.Defects()) != 0 {
		t.Fatalf("recorder loss is inconclusive, not a defect: %v", broken.Defects())
	}
	if len(broken.MissingFromDelivery) != 2 {
		t.Fatalf("the undelivered rows must still be recorded: %+v", broken)
	}

	never := Compare(nil, persisted, scope(1, 3), Connection{Established: false, Status: 500})
	if never.Conclusive() {
		t.Fatal("a subscription that never opened must be inconclusive")
	}
}

func open() Connection { return Connection{Established: true, Status: 200} }

func TestReconnectReplayBelowCursorIsLegal(t *testing.T) {
	persisted := []Persisted{{Sequence: 10}, {Sequence: 11}, {Sequence: 12}}
	first := []Delivered{{Sequence: 10}, {Sequence: 11}}
	resumed := []Delivered{{Sequence: 11}, {Sequence: 12}}

	rep := CompareReconnect(first, resumed, persisted, 11, scope(10, 12), open())
	if len(rep.ReplayedAtOrBelowCursor) != 1 || rep.ReplayedAtOrBelowCursor[0] != 11 {
		t.Fatalf("replayed duplicate not recorded: %+v", rep)
	}
	if len(rep.NewAboveCursor) != 1 || rep.NewAboveCursor[0] != 12 {
		t.Fatalf("resumed events above the cursor not recorded: %+v", rep)
	}
	if len(rep.Defects()) != 0 {
		t.Fatalf("legal reconnection reported defects: %v", rep.Defects())
	}
	if !rep.Conclusive() {
		t.Fatalf("an established reconnection with real catch-up should be conclusive: %v", rep.Inconclusive)
	}
}

// The catch-up above the cursor is read from the durable store, not delivered
// at-least-once, so a persisted row the resume did not replay is a defect.
func TestReconnectDidNotReplayPersistedRowIsADefect(t *testing.T) {
	persisted := []Persisted{{Sequence: 10}, {Sequence: 11}, {Sequence: 12}}
	first := []Delivered{{Sequence: 10}}
	resumed := []Delivered{{Sequence: 12}}

	rep := CompareReconnect(first, resumed, persisted, 10, scope(10, 12), open())
	if len(rep.MissingFromResumed) != 1 || rep.MissingFromResumed[0] != 11 {
		t.Fatalf("unreplayed persisted row not recorded: %+v", rep)
	}
	if len(rep.PersistedAboveCursorNeverDelivered) != 1 || rep.PersistedAboveCursorNeverDelivered[0] != 11 {
		t.Fatalf("never-delivered persisted row not recorded: %+v", rep)
	}
	if len(rep.Defects()) == 0 {
		t.Fatal("a resume that skipped a persisted row above its cursor must be a defect")
	}
}

func TestResumedDeliveryWithoutPersistedRowIsADefect(t *testing.T) {
	rep := CompareReconnect(nil, []Delivered{{Sequence: 42}}, []Persisted{{Sequence: 40}}, 10, scope(10, 50), open())
	if len(rep.ResumedNotPersisted) != 1 || rep.ResumedNotPersisted[0] != 42 {
		t.Fatalf("planted uncommitted resumed delivery not detected: %+v", rep)
	}
	if len(rep.Defects()) == 0 {
		t.Fatalf("planted uncommitted resumed delivery produced no defect: %+v", rep)
	}
}

// The P1 this comparison exists to close: a reconnection that never opened
// delivers an empty set, which must never read as a successful comparison.
func TestReconnectThatNeverOpenedIsInconclusive(t *testing.T) {
	persisted := []Persisted{{Sequence: 10}, {Sequence: 11}}
	rep := CompareReconnect([]Delivered{{Sequence: 10}}, nil, persisted, 10, scope(10, 11),
		Connection{Established: false, Status: 403, Err: "/v1/events status 403: forbidden"})
	if rep.Conclusive() {
		t.Fatalf("a reconnection that never opened must be inconclusive: %+v", rep)
	}
}

func TestReconnectThatDeliveredNothingIsInconclusive(t *testing.T) {
	persisted := []Persisted{{Sequence: 10}, {Sequence: 11}}
	rep := CompareReconnect([]Delivered{{Sequence: 10}}, nil, persisted, 10, scope(10, 11), open())
	if rep.Conclusive() {
		t.Fatalf("an empty resumed set must be inconclusive: %+v", rep)
	}
}

// Reconnecting at the tip exercises no catch-up at all, which is exactly what
// the first live evidence recorded. That is inconclusive, not a pass.
func TestReconnectWithNoCatchUpIsInconclusive(t *testing.T) {
	persisted := []Persisted{{Sequence: 10}, {Sequence: 11}}
	rep := CompareReconnect([]Delivered{{Sequence: 10}, {Sequence: 11}}, nil, persisted, 11, scope(10, 11), open())
	if len(rep.PersistedAboveCursor) != 0 {
		t.Fatalf("cursor at the tip should leave no catch-up: %+v", rep)
	}
	if rep.Conclusive() {
		t.Fatalf("a reconnection with nothing to catch up on must be inconclusive: %+v", rep)
	}
	if len(rep.Defects()) != 0 {
		t.Fatalf("inconclusive must not also be a defect: %v", rep.Defects())
	}
}

func TestReconnectParserFailureIsInconclusive(t *testing.T) {
	persisted := []Persisted{{Sequence: 10}, {Sequence: 11}}
	rep := CompareReconnect(nil, []Delivered{{Sequence: 11}}, persisted, 10, scope(10, 11),
		Connection{Established: true, Status: 200, Err: "unexpected EOF"})
	if rep.Conclusive() {
		t.Fatalf("a stream that ended in a parser failure must be inconclusive: %+v", rep)
	}
}

// The P2 this closes: a mixed valid/malformed stream still delivers some
// events, so the missing row looks like legal at-least-once loss unless the
// decode-failure count on the connection makes the comparison inconclusive.
func TestPayloadDecodeFailureIsInconclusiveEvenWhenOtherEventsDecode(t *testing.T) {
	persisted := []Persisted{{Sequence: 1, Type: "run_started"}, {Sequence: 2, Type: "task_started"}}
	delivered := []Delivered{{Sequence: 1, Type: "run_started"}}

	healthy := Compare(delivered, persisted, scope(1, 2), open())
	if !healthy.Conclusive() {
		t.Fatalf("a live delivery gap on a healthy stream is legal: %v", healthy.Inconclusive)
	}

	broken := Compare(delivered, persisted, scope(1, 2),
		Connection{Established: true, Status: 200, DecodeFailures: 1})
	if broken.Conclusive() {
		t.Fatalf("a stream that failed to decode a payload must be inconclusive: %+v", broken)
	}
	if len(broken.Defects()) != 0 {
		t.Fatalf("recorder/parser loss is inconclusive, not a defect: %v", broken.Defects())
	}
	if len(broken.MissingFromDelivery) != 1 || broken.MissingFromDelivery[0] != 2 {
		t.Fatalf("the undecoded row must still be recorded as missing: %+v", broken)
	}

	resumed := CompareReconnect(delivered, []Delivered{{Sequence: 2, Type: "task_started"}}, persisted, 1, scope(1, 2),
		Connection{Established: true, Status: 200, DecodeFailures: 1})
	if resumed.Conclusive() {
		t.Fatalf("a reconnection that failed to decode a payload must be inconclusive: %+v", resumed)
	}
	if len(resumed.Defects()) != 0 {
		t.Fatalf("reconnect decoder loss is inconclusive, not a defect: %v", resumed.Defects())
	}
}

// The clause this exists for: SSE cannot reveal an external effect whose
// completion event was lost, so a raw completion with no delivered event must
// be reported and must not fail the run.
func TestEffectLedgerSurvivesLostCompletionEvent(t *testing.T) {
	effects := []Effect{
		{RunID: "r", Step: "block", Nonce: "n1", Kind: "start"},
		{RunID: "r", Step: "block", Nonce: "n1", Kind: "complete"},
		{RunID: "r", Step: "successor", Nonce: "n2", Kind: "start"},
		{RunID: "r", Step: "successor", Nonce: "n2", Kind: "complete"},
	}
	delivered := []Delivered{{Sequence: 5, Type: "task_succeeded", RunID: "r", TaskID: "t1"}}
	persisted := []Persisted{
		{Sequence: 5, Type: "task_succeeded", RunID: "r", TaskID: "t1"},
		{Sequence: 6, Type: "task_succeeded", RunID: "r", TaskID: "t2"},
	}

	rep := CorrelateEffects("r", effects, delivered, persisted, []string{"task_succeeded", "task_failed"}, fixtureSteps)
	if rep.RawCompletions != 2 {
		t.Fatalf("raw completions lost: %+v", rep)
	}
	if rep.UnwitnessedCompletions != 1 {
		t.Fatalf("lost completion event not reported: %+v", rep)
	}
	if rep.PerStep["successor"].UnwitnessedCompletions != 1 {
		t.Fatalf("the lost event was not attributed to its own step: %+v", rep.PerStep)
	}
	if len(rep.Defects()) != 0 {
		t.Fatalf("a lost completion event is not a defect: %v", rep.Defects())
	}
	if !rep.Conclusive() {
		t.Fatalf("a populated ledger should be conclusive: %v", rep.Inconclusive)
	}
}

// Negative control in the other direction: reporting more completions than
// externally happened is a real defect.
func TestPhantomCompletionIsADefect(t *testing.T) {
	effects := []Effect{
		{RunID: "r", Step: "block", Nonce: "n1", Kind: "start"},
		{RunID: "r", Step: "block", Nonce: "n1", Kind: "complete"},
	}
	persisted := []Persisted{
		{Sequence: 5, Type: "task_succeeded", RunID: "r", TaskID: "t1"},
		{Sequence: 6, Type: "task_succeeded", RunID: "r", TaskID: "t1"},
		{Sequence: 7, Type: "task_succeeded", RunID: "r", TaskID: "t1"},
	}
	rep := CorrelateEffects("r", effects, nil, persisted, []string{"task_succeeded"}, fixtureSteps)
	if rep.PhantomCompletions != 2 {
		t.Fatalf("phantom completions not counted: %+v", rep)
	}
	if len(rep.Defects()) == 0 {
		t.Fatal("phantom completions did not produce a defect")
	}
}

// The defect the run-wide comparison could not see: `block` ran twice and
// `successor` never ran externally, yet both aggregates are 2 == 2. Only a
// per-task comparison catches `successor`'s phantom completion.
func TestEqualAggregatesAcrossDifferentTasksIsADefect(t *testing.T) {
	effects := []Effect{
		{RunID: "r", Step: "block", Nonce: "n1", Kind: "start"},
		{RunID: "r", Step: "block", Nonce: "n1", Kind: "complete"},
		{RunID: "r", Step: "block", Nonce: "n2", Kind: "complete"},
	}
	persisted := []Persisted{
		{Sequence: 5, Type: "task_succeeded", RunID: "r", TaskID: "t1"},
		{Sequence: 6, Type: "task_succeeded", RunID: "r", TaskID: "t2"},
	}

	rep := CorrelateEffects("r", effects, nil, persisted, []string{"task_succeeded"}, fixtureSteps)
	if rep.RawCompletions != rep.PersistedCompletionEvents {
		t.Fatalf("this fixture is only interesting when the aggregates match: %+v", rep)
	}
	if rep.PerStep["block"].RawCompletions != 2 {
		t.Fatalf("raw duplicates must be retained per step: %+v", rep.PerStep)
	}
	if rep.PerStep["successor"].PhantomCompletions != 1 {
		t.Fatalf("the phantom completion of `successor` was not detected: %+v", rep.PerStep)
	}
	if len(rep.Defects()) == 0 {
		t.Fatal("equal run-wide totals hid a per-task phantom completion")
	}
}

func TestEmptyLedgerIsInconclusive(t *testing.T) {
	rep := CorrelateEffects("r", nil, nil, nil, []string{"task_succeeded"}, fixtureSteps)
	if rep.Conclusive() {
		t.Fatal("an empty effect ledger must be inconclusive, never a pass")
	}
	if len(rep.Defects()) != 0 {
		t.Fatalf("an empty ledger is not a defect: %v", rep.Defects())
	}
}

// A completion event whose task identity resolves to no known step cannot be
// compared per task, so it must not quietly fall back to a run-wide total.
func TestUnmappedTaskIdentityIsInconclusive(t *testing.T) {
	effects := []Effect{
		{RunID: "r", Step: "block", Nonce: "n1", Kind: "start"},
		{RunID: "r", Step: "block", Nonce: "n1", Kind: "complete"},
	}
	persisted := []Persisted{{Sequence: 5, Type: "task_succeeded", RunID: "r", TaskID: "t-unknown"}}
	rep := CorrelateEffects("r", effects, nil, persisted, []string{"task_succeeded"}, fixtureSteps)
	if rep.UnidentifiedCompletionEvents != 1 {
		t.Fatalf("unmapped completion event not counted: %+v", rep)
	}
	if rep.Conclusive() {
		t.Fatalf("an unmapped task identity must be inconclusive: %+v", rep)
	}
}

func TestMissingTaskIdentityMapIsInconclusive(t *testing.T) {
	effects := []Effect{
		{RunID: "r", Step: "block", Nonce: "n1", Kind: "start"},
		{RunID: "r", Step: "block", Nonce: "n1", Kind: "complete"},
	}
	rep := CorrelateEffects("r", effects, nil, nil, []string{"task_succeeded"}, nil)
	if rep.Conclusive() {
		t.Fatal("no task identity map means nothing can be compared per task")
	}
}

func TestEffectsFromOtherRunsAreIgnored(t *testing.T) {
	effects := []Effect{
		{RunID: "other", Kind: "start"},
		{RunID: "other", Kind: "complete"},
		{RunID: "r", Step: "block", Kind: "start"},
	}
	rep := CorrelateEffects("r", effects, nil, nil, []string{"task_succeeded"}, fixtureSteps)
	if rep.RawStarts != 1 || rep.RawCompletions != 0 {
		t.Fatalf("effects were not scoped to the selected run: %+v", rep)
	}
}
