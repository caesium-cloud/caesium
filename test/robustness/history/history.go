// Package history correlates the three independent histories the robustness
// harness keeps for one run, under the sequence scope DT-EVENT-01 actually
// documents.
//
// The three histories are deliberately not interchangeable:
//
//  1. Delivered — what the public `/v1/events` SSE stream handed a subscriber.
//     At-least-once, so duplicates and out-of-order arrivals are LEGAL, and a
//     delivery can be lost outright (an in-process subscriber buffer overflow
//     drops delivery even for a row already marked dispatched).
//  2. Persisted — rows actually read back from the event store. This is
//     implementation-produced too, but it is the durable one.
//  3. The raw task-effect ledger — what external task processes told the
//     recorder they did. SSE cannot reveal an external effect whose completion
//     event was lost, so this ledger is kept separately and is never replaced
//     by event evidence.
//
// The comparison is therefore a SET comparison inside an explicitly recorded
// scope. `Sequence` identifies an event within the selected store's lifetime;
// it is not a gap-free global counter, per-run sequences are sparse by
// construction, and no high-water mark here means "everything below arrived".
// A scope that was not fully read, or a history that is missing, yields
// Inconclusive — never a pass and never a failure.
package history

import (
	"fmt"
	"sort"
)

// Delivered is one observed SSE delivery.
type Delivered struct {
	Sequence uint64
	Type     string
	RunID    string
	// TaskID is the event's own task identity. Effect correlation is per task:
	// run-wide counts let one task's duplicate completions conceal another
	// task's phantom completion.
	TaskID  string
	ConnGen int
}

// Persisted is one row read back from the event store.
type Persisted struct {
	Sequence uint64
	Type     string
	RunID    string
	TaskID   string
}

// Scope records which store was read and which sequence range that read
// actually covered. Comparisons are only meaningful inside it.
type Scope struct {
	// Store identifies the read surface (member address, namespace, ...).
	Store string
	// Min and Max are the inclusive sequence bounds the persisted read covered.
	Min, Max uint64
	// Complete is false when the read was truncated, filtered further than the
	// delivery filter, or otherwise could not cover the range.
	Complete bool
}

func (s Scope) contains(seq uint64) bool {
	if seq == 0 {
		return false
	}
	if s.Min != 0 && seq < s.Min {
		return false
	}
	if s.Max != 0 && seq > s.Max {
		return false
	}
	return true
}

// Report is the outcome of comparing delivery against persistence.
//
// Only Defects() is a failure. Everything else is retained evidence: duplicates
// and undelivered persisted rows are legal under DT-EVENT-01, and unidentified
// or out-of-scope deliveries make that part of the comparison inconclusive.
type Report struct {
	Scope Scope
	// Conn is the subscription that produced the delivered set. A broken
	// recorder cannot be told apart from legal delivery loss without it.
	Conn                  Connection
	DeliveredCount        int
	DeliveredDistinct     int
	PersistedCount        int
	Duplicates            map[uint64]int
	MissingFromDelivery   []uint64
	DeliveredNotPersisted []uint64
	TypeMismatch          []uint64
	OutOfScope            []uint64
	Unidentified          int
	OutOfOrder            int
	Inconclusive          []string
}

// Compare performs the set comparison described in the package doc.
//
// conn is the subscription that produced `delivered`. It is required because a
// recorder that delivered one event and then broke still yields a non-empty
// set, and every row it never saw would otherwise be classified as legal
// at-least-once loss. Recorder loss is INCONCLUSIVE, not a pass: a stream that
// never reached HTTP 200, ended before the caller closed it, or failed its
// parser makes the whole comparison inconclusive.
func Compare(delivered []Delivered, persisted []Persisted, scope Scope, conn Connection) Report {
	rep := Report{
		Scope:          scope,
		Conn:           conn,
		DeliveredCount: len(delivered),
		PersistedCount: len(persisted),
		Duplicates:     map[uint64]int{},
	}

	persistedBySeq := make(map[uint64]Persisted, len(persisted))
	for _, p := range persisted {
		if p.Sequence == 0 {
			rep.Inconclusive = append(rep.Inconclusive, "persisted row with sequence 0")
			continue
		}
		persistedBySeq[p.Sequence] = p
	}

	counts := map[uint64]int{}
	var lastSeq uint64
	for _, d := range delivered {
		if d.Sequence == 0 {
			rep.Unidentified++
			continue
		}
		if lastSeq != 0 && d.Sequence < lastSeq {
			rep.OutOfOrder++
		}
		lastSeq = d.Sequence
		counts[d.Sequence]++
		if !scope.contains(d.Sequence) {
			rep.OutOfScope = append(rep.OutOfScope, d.Sequence)
			continue
		}
		p, ok := persistedBySeq[d.Sequence]
		if !ok {
			rep.DeliveredNotPersisted = append(rep.DeliveredNotPersisted, d.Sequence)
			continue
		}
		if d.Type != "" && p.Type != "" && d.Type != p.Type {
			rep.TypeMismatch = append(rep.TypeMismatch, d.Sequence)
		}
	}
	rep.DeliveredDistinct = len(counts)
	for seq, n := range counts {
		if n > 1 {
			rep.Duplicates[seq] = n
		}
	}

	for seq := range persistedBySeq {
		if !scope.contains(seq) {
			continue
		}
		if counts[seq] == 0 {
			rep.MissingFromDelivery = append(rep.MissingFromDelivery, seq)
		}
	}

	if !scope.Complete {
		rep.Inconclusive = append(rep.Inconclusive,
			"persisted read did not cover the selected scope")
	}
	if len(persisted) == 0 {
		rep.Inconclusive = append(rep.Inconclusive,
			"no persisted rows were read for the selected scope")
	}
	if rep.Unidentified > 0 {
		rep.Inconclusive = append(rep.Inconclusive,
			fmt.Sprintf("%d deliveries carried no event id", rep.Unidentified))
	}
	if len(rep.OutOfScope) > 0 {
		rep.Inconclusive = append(rep.Inconclusive,
			fmt.Sprintf("%d deliveries fell outside the read scope", len(rep.OutOfScope)))
	}
	if !conn.Established {
		rep.Inconclusive = append(rep.Inconclusive, fmt.Sprintf(
			"the subscription was never established (status=%d error=%q): what it did not deliver cannot be called legal loss",
			conn.Status, conn.Err))
	}
	if conn.Err != "" {
		rep.Inconclusive = append(rep.Inconclusive, fmt.Sprintf(
			"the subscription did not survive the observation window: %s (recorder loss is inconclusive, never legal at-least-once loss)",
			conn.Err))
	}

	sortU64(rep.MissingFromDelivery)
	sortU64(rep.DeliveredNotPersisted)
	sortU64(rep.TypeMismatch)
	sortU64(rep.OutOfScope)
	return rep
}

// Defects returns only the findings that are illegal under DT-EVENT-01.
//
// A delivered event with no durable row in the same scope means the system
// published something it had not committed. A type that disagrees between the
// two histories means one of them is wrong about the same identity. Missing
// deliveries, duplicates and reordering are explicitly NOT defects.
func (r Report) Defects() []string {
	var out []string
	if len(r.DeliveredNotPersisted) > 0 {
		out = append(out, fmt.Sprintf(
			"delivered events with no persisted row in scope %v: %v",
			r.Scope, r.DeliveredNotPersisted))
	}
	if len(r.TypeMismatch) > 0 {
		out = append(out, fmt.Sprintf(
			"delivered/persisted type disagreement at sequences %v", r.TypeMismatch))
	}
	return out
}

// Conclusive reports whether the comparison can support any claim at all.
func (r Report) Conclusive() bool { return len(r.Inconclusive) == 0 }

// Connection is the caller's record of the resumed subscription attempt. A
// stream that never opened, or that ended with a transport/parser failure,
// delivers an empty set — which is indistinguishable from "the server replayed
// nothing" unless the attempt itself is carried into the comparison. So it is.
type Connection struct {
	// Established is true only when an HTTP 200 event stream was actually
	// opened.
	Established bool
	// Status is the observed HTTP status (0 when the request never completed).
	Status int
	// Err is a connection or parser failure that was NOT the caller's own
	// cancellation. Any value makes the comparison inconclusive.
	Err string
}

// ReconnectReport compares a resumed subscription with the first one.
type ReconnectReport struct {
	Cursor uint64
	// Conn is the resumed subscription attempt itself.
	Conn Connection
	// FirstDistinct and ResumedDistinct are set sizes, not ranges.
	FirstDistinct   int
	ResumedDistinct int
	// PersistedAboveCursor is the catch-up the reconnection had available: the
	// persisted rows in scope strictly above the cursor. An empty set means the
	// reconnection exercised nothing and can support no claim.
	PersistedAboveCursor []uint64
	// MissingFromResumed are persisted rows above the cursor that the resumed
	// stream did not deliver. Unlike a LIVE delivery gap this is a defect: the
	// documented `Last-Event-ID` catch-up is served from the durable store, so
	// a row the store holds above the cursor must be replayed.
	MissingFromResumed []uint64
	// ReplayedAtOrBelowCursor are resumed deliveries the cursor already
	// covered. At-least-once delivery makes these legal; they are recorded.
	ReplayedAtOrBelowCursor []uint64
	// NewAboveCursor are resumed deliveries above the cursor.
	NewAboveCursor []uint64
	// PersistedAboveCursorNeverDelivered are rows the store holds above the
	// cursor that neither connection delivered. Legal, and precisely why the
	// raw effect ledger is preserved separately.
	PersistedAboveCursorNeverDelivered []uint64
	// ResumedNotPersisted is a defect: a resumed delivery in scope with no row.
	ResumedNotPersisted []uint64
	Inconclusive        []string
}

// CompareReconnect compares a reconnection's results with the first stream and
// the persisted rows, using the cursor that was actually sent as
// `Last-Event-ID`. It makes no gap-free assumption: everything is a set.
//
// conn carries the subscription attempt, because an empty resumed set proves
// nothing on its own — a 403, a transport error or a cancelled dial all produce
// one. A reconnection that was not established, delivered nothing, or had no
// persisted catch-up above the cursor is INCONCLUSIVE, never a pass.
func CompareReconnect(first, resumed []Delivered, persisted []Persisted, cursor uint64, scope Scope, conn Connection) ReconnectReport {
	rep := ReconnectReport{Cursor: cursor, Conn: conn}

	firstSet := map[uint64]struct{}{}
	for _, d := range first {
		if d.Sequence != 0 {
			firstSet[d.Sequence] = struct{}{}
		}
	}
	rep.FirstDistinct = len(firstSet)

	persistedSet := map[uint64]struct{}{}
	for _, p := range persisted {
		if p.Sequence != 0 {
			persistedSet[p.Sequence] = struct{}{}
		}
	}

	resumedSet := map[uint64]struct{}{}
	for _, d := range resumed {
		if d.Sequence == 0 {
			rep.Inconclusive = append(rep.Inconclusive, "resumed delivery carried no event id")
			continue
		}
		resumedSet[d.Sequence] = struct{}{}
	}
	rep.ResumedDistinct = len(resumedSet)

	for seq := range resumedSet {
		if seq <= cursor {
			rep.ReplayedAtOrBelowCursor = append(rep.ReplayedAtOrBelowCursor, seq)
		} else {
			rep.NewAboveCursor = append(rep.NewAboveCursor, seq)
		}
		if scope.contains(seq) {
			if _, ok := persistedSet[seq]; !ok {
				rep.ResumedNotPersisted = append(rep.ResumedNotPersisted, seq)
			}
		}
	}
	for seq := range persistedSet {
		if seq <= cursor || !scope.contains(seq) {
			continue
		}
		rep.PersistedAboveCursor = append(rep.PersistedAboveCursor, seq)
		_, inResumed := resumedSet[seq]
		_, inFirst := firstSet[seq]
		if !inResumed {
			rep.MissingFromResumed = append(rep.MissingFromResumed, seq)
		}
		if !inResumed && !inFirst {
			rep.PersistedAboveCursorNeverDelivered = append(rep.PersistedAboveCursorNeverDelivered, seq)
		}
	}
	if !scope.Complete {
		rep.Inconclusive = append(rep.Inconclusive, "persisted read did not cover the selected scope")
	}
	if !conn.Established {
		rep.Inconclusive = append(rep.Inconclusive, fmt.Sprintf(
			"the resumed stream was never established (status=%d error=%q): an empty resumed set is evidence of nothing",
			conn.Status, conn.Err))
	}
	if conn.Err != "" {
		rep.Inconclusive = append(rep.Inconclusive, fmt.Sprintf(
			"the resumed stream failed: %s", conn.Err))
	}
	if len(rep.PersistedAboveCursor) == 0 {
		rep.Inconclusive = append(rep.Inconclusive,
			"no persisted rows above the cursor: the reconnection had no catch-up to exercise")
	}
	if rep.ResumedDistinct == 0 {
		rep.Inconclusive = append(rep.Inconclusive,
			"the resumed stream delivered nothing, so no reconnection result was compared")
	}

	sortU64(rep.ReplayedAtOrBelowCursor)
	sortU64(rep.NewAboveCursor)
	sortU64(rep.PersistedAboveCursor)
	sortU64(rep.MissingFromResumed)
	sortU64(rep.PersistedAboveCursorNeverDelivered)
	sortU64(rep.ResumedNotPersisted)
	return rep
}

// Defects returns only the illegal findings of a reconnection comparison.
//
// MissingFromResumed is a defect while a live delivery gap is not: the
// `Last-Event-ID` catch-up is served from the durable store, so a persisted row
// above the cursor that the resumed stream never replayed is a broken resume,
// not at-least-once delivery.
func (r ReconnectReport) Defects() []string {
	var out []string
	if len(r.ResumedNotPersisted) > 0 {
		out = append(out, fmt.Sprintf(
			"resumed deliveries with no persisted row: %v", r.ResumedNotPersisted))
	}
	if len(r.MissingFromResumed) > 0 {
		out = append(out, fmt.Sprintf(
			"the resume from cursor %d did not replay persisted rows %v (catch-up is read from the durable store, so this is not at-least-once delivery loss)",
			r.Cursor, r.MissingFromResumed))
	}
	return out
}

// Conclusive reports whether the reconnection comparison can support a claim.
func (r ReconnectReport) Conclusive() bool { return len(r.Inconclusive) == 0 }

// Effect is one raw task-effect ledger record from the recorder sink. The
// external process wrote it; no event had to exist for it to be true.
type Effect struct {
	RunID string
	Step  string
	Nonce string
	Kind  string // "start" or "complete"
}

// StepCorrelation is one fixture step's own correlation. Counts are kept per
// step because run-wide totals are forgeable: two completions of `first` and
// none of `second` sum to the same total as one of each, so an aggregate
// comparison cannot see `second`'s phantom completion.
type StepCorrelation struct {
	Step string `json:"step"`
	// RawCompletions retains duplicates: the ledger's raw records are evidence,
	// and collapsing them would hide a double execution.
	RawCompletions            int `json:"raw_completions"`
	RawStarts                 int `json:"raw_starts"`
	DistinctCompletedNonces   int `json:"distinct_completed_nonces"`
	DeliveredCompletionEvents int `json:"delivered_completion_events"`
	PersistedCompletionEvents int `json:"persisted_completion_events"`
	UnwitnessedCompletions    int `json:"unwitnessed_completions"`
	PhantomCompletions        int `json:"phantom_completions"`
}

// EffectReport correlates the raw ledger with the event histories, per task.
type EffectReport struct {
	RunID                     string `json:"run_id"`
	RawStarts                 int    `json:"raw_starts"`
	RawCompletions            int    `json:"raw_completions"`
	DistinctCompletedNonces   int    `json:"distinct_completed_nonces"`
	DeliveredCompletionEvents int    `json:"delivered_completion_events"`
	PersistedCompletionEvents int    `json:"persisted_completion_events"`
	// UnwitnessedCompletions counts raw completions with no delivered
	// completion event, summed over steps. This is NOT a defect: it is exactly
	// the case the separate ledger exists for.
	UnwitnessedCompletions int `json:"unwitnessed_completions"`
	// PhantomCompletions counts persisted completion events in excess of the
	// raw external completions FOR THE SAME STEP. That IS a defect: the system
	// reported work that left no external trace.
	PhantomCompletions int `json:"phantom_completions"`
	// PerStep is the comparison that decides the report.
	PerStep map[string]StepCorrelation `json:"per_step"`
	// UnidentifiedCompletionEvents counts completion events whose task identity
	// maps to no known step. They cannot be compared per task, so they make the
	// report inconclusive rather than silently folding back into a total.
	UnidentifiedCompletionEvents int      `json:"unidentified_completion_events"`
	UnmappedTaskIDs              []string `json:"unmapped_task_ids,omitempty"`
	Notes                        []string `json:"notes,omitempty"`
	Inconclusive                 []string `json:"inconclusive,omitempty"`
}

// CorrelateEffects compares the raw effect ledger with the two event histories
// for one run, per task. completionTypes are the event types that assert a task
// finished (e.g. task_succeeded, task_failed). taskSteps maps the catalog task
// identity carried by those events to the fixture step that produced the raw
// effect records; without it nothing can be compared per task and the report is
// inconclusive.
func CorrelateEffects(runID string, effects []Effect, delivered []Delivered, persisted []Persisted, completionTypes []string, taskSteps map[string]string) EffectReport {
	rep := EffectReport{RunID: runID, PerStep: map[string]StepCorrelation{}}
	wanted := map[string]struct{}{}
	for _, t := range completionTypes {
		wanted[t] = struct{}{}
	}

	steps := map[string]*StepCorrelation{}
	step := func(name string) *StepCorrelation {
		if s, ok := steps[name]; ok {
			return s
		}
		s := &StepCorrelation{Step: name}
		steps[name] = s
		return s
	}

	noncesByStep := map[string]map[string]struct{}{}
	for _, e := range effects {
		if e.RunID != runID {
			continue
		}
		name := e.Step
		if name == "" {
			name = "(unnamed step)"
		}
		switch e.Kind {
		case "start":
			rep.RawStarts++
			step(name).RawStarts++
		case "complete":
			rep.RawCompletions++
			step(name).RawCompletions++
			if e.Nonce != "" {
				if noncesByStep[name] == nil {
					noncesByStep[name] = map[string]struct{}{}
				}
				noncesByStep[name][e.Nonce] = struct{}{}
			}
		}
	}
	distinct := map[string]struct{}{}
	for name, set := range noncesByStep {
		step(name).DistinctCompletedNonces = len(set)
		for nonce := range set {
			distinct[nonce] = struct{}{}
		}
	}
	rep.DistinctCompletedNonces = len(distinct)

	unmapped := map[string]struct{}{}
	// resolve attributes one completion event to a step, or records that its
	// identity could not be resolved. Sequences are de-duplicated per step so
	// one row delivered twice is one event, while the RAW ledger keeps its
	// duplicates.
	resolve := func(taskID string, seq uint64, seen map[string]map[uint64]struct{}) {
		name, ok := taskSteps[taskID]
		if !ok || name == "" {
			rep.UnidentifiedCompletionEvents++
			if taskID != "" {
				unmapped[taskID] = struct{}{}
			} else {
				unmapped["(no task id)"] = struct{}{}
			}
			return
		}
		if seen[name] == nil {
			seen[name] = map[uint64]struct{}{}
		}
		seen[name][seq] = struct{}{}
	}

	deliveredSeen := map[string]map[uint64]struct{}{}
	for _, d := range delivered {
		if d.RunID != "" && d.RunID != runID {
			continue
		}
		if _, ok := wanted[d.Type]; !ok {
			continue
		}
		if d.Sequence == 0 {
			continue
		}
		resolve(d.TaskID, d.Sequence, deliveredSeen)
	}
	persistedSeen := map[string]map[uint64]struct{}{}
	for _, p := range persisted {
		if p.RunID != "" && p.RunID != runID {
			continue
		}
		if _, ok := wanted[p.Type]; !ok {
			continue
		}
		resolve(p.TaskID, p.Sequence, persistedSeen)
	}

	for name, seqs := range deliveredSeen {
		s := step(name)
		s.DeliveredCompletionEvents = len(seqs)
	}
	for name, seqs := range persistedSeen {
		s := step(name)
		s.PersistedCompletionEvents = len(seqs)
	}

	for name, s := range steps {
		if s.RawCompletions > s.DeliveredCompletionEvents {
			s.UnwitnessedCompletions = s.RawCompletions - s.DeliveredCompletionEvents
			rep.Notes = append(rep.Notes, fmt.Sprintf(
				"step %s: %d external completions have no delivered completion event; the raw ledger is the only witness (legal under DT-EVENT-01)",
				name, s.UnwitnessedCompletions))
		}
		if s.PersistedCompletionEvents > s.RawCompletions {
			s.PhantomCompletions = s.PersistedCompletionEvents - s.RawCompletions
		}
		rep.DeliveredCompletionEvents += s.DeliveredCompletionEvents
		rep.PersistedCompletionEvents += s.PersistedCompletionEvents
		rep.UnwitnessedCompletions += s.UnwitnessedCompletions
		rep.PhantomCompletions += s.PhantomCompletions
		rep.PerStep[name] = *s
	}

	for id := range unmapped {
		rep.UnmappedTaskIDs = append(rep.UnmappedTaskIDs, id)
	}
	sort.Strings(rep.UnmappedTaskIDs)
	sort.Strings(rep.Notes)

	if rep.RawStarts == 0 {
		rep.Inconclusive = append(rep.Inconclusive,
			"no raw effect records for this run: correlation is inconclusive")
	}
	if len(taskSteps) == 0 {
		rep.Inconclusive = append(rep.Inconclusive,
			"no task identity map was supplied: completion evidence cannot be compared per task")
	}
	if rep.UnidentifiedCompletionEvents > 0 {
		rep.Inconclusive = append(rep.Inconclusive, fmt.Sprintf(
			"%d completion events carried a task identity that maps to no known step (%v)",
			rep.UnidentifiedCompletionEvents, rep.UnmappedTaskIDs))
	}
	return rep
}

// Defects returns only the illegal findings of an effect correlation.
func (r EffectReport) Defects() []string {
	var out []string
	names := make([]string, 0, len(r.PerStep))
	for name := range r.PerStep {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		s := r.PerStep[name]
		if s.PhantomCompletions > 0 {
			out = append(out, fmt.Sprintf(
				"step %s of run %s: %d persisted completion events exceed the %d external completions actually recorded for THAT step",
				name, r.RunID, s.PhantomCompletions, s.RawCompletions))
		}
	}
	return out
}

// Conclusive reports whether the correlation can support a claim.
func (r EffectReport) Conclusive() bool { return len(r.Inconclusive) == 0 }

func sortU64(v []uint64) {
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
}
