package model

import (
	"fmt"
	"sort"
)

// Lease is one run's ownership record: who holds it, until when, and under
// which generation.
//
// The generation is the fence. It is not a clock, not a term number agreed by
// consensus, and not a promise that the previous holder has stopped: it is a
// counter that strictly increases on every takeover, so a message carrying an
// older one can be identified as stale even when its sender still believes it
// is the owner.
type Lease struct {
	RunID       string
	Owner       string
	Generation  int64
	AcquiredMs  int64
	ExpiresAtMs int64
}

// Expired reports whether the lease has lapsed at a given instant. Expiry is
// evaluated against node wall clocks in the product, so a model that assumes a
// single clock is modelling the no-skew case only — which is exactly the case
// the first real crash scenario runs, and it is stated rather than implied.
func (l Lease) Expired(nowMs int64) bool { return l.ExpiresAtMs <= nowMs }

// LeaseTable is the modelled run_leases table.
type LeaseTable struct {
	leases map[string]Lease
	// history retains every generation ever observed per run so the oracle can
	// check strict monotonicity rather than only the current value.
	history map[string][]int64
}

// NewLeaseTable builds an empty table.
func NewLeaseTable() *LeaseTable {
	return &LeaseTable{leases: map[string]Lease{}, history: map[string][]int64{}}
}

// Acquire writes a lease for a run that has none. It is idempotent: whoever
// wrote first is the owner, and a second acquisition returns the existing
// generation rather than stealing the run. Reports the generation in force and
// whether this call is the one that created it.
func (t *LeaseTable) Acquire(runID, owner string, nowMs, ttlMs int64) (int64, bool) {
	if existing, ok := t.leases[runID]; ok {
		return existing.Generation, false
	}
	l := Lease{
		RunID: runID, Owner: owner, Generation: 1,
		AcquiredMs: nowMs, ExpiresAtMs: nowMs + ttlMs,
	}
	t.leases[runID] = l
	t.history[runID] = append(t.history[runID], 1)
	return 1, true
}

// Renew extends a lease still held by owner. The owner predicate is the safety
// net: a holder that was taken over between deciding to renew and writing must
// not extend a lease it no longer holds.
func (t *LeaseTable) Renew(runID, owner string, newExpiresAtMs int64) bool {
	l, ok := t.leases[runID]
	if !ok || l.Owner != owner {
		return false
	}
	l.ExpiresAtMs = newExpiresAtMs
	t.leases[runID] = l
	return true
}

// RenewOwned extends every unexpired lease owner holds, and reports how many.
func (t *LeaseTable) RenewOwned(owner string, nowMs, newExpiresAtMs int64) int {
	n := 0
	for runID, l := range t.leases {
		if l.Owner != owner || l.Expired(nowMs) {
			continue
		}
		l.ExpiresAtMs = newExpiresAtMs
		t.leases[runID] = l
		n++
	}
	return n
}

// AcquireExpired takes over every lease whose holder let it lapse, reassigning
// it to newOwner with an incremented generation and a fresh expiry.
//
// The expiry predicate IS the compare-and-swap. Two nodes sweeping
// concurrently cannot both take the same run over: the first write moves the
// row out of the expired set, so the second matches nothing. That is why the
// model applies the whole sweep atomically rather than per row.
func (t *LeaseTable) AcquireExpired(newOwner string, nowMs, ttlMs int64) []string {
	ids := make([]string, 0, len(t.leases))
	for runID := range t.leases {
		ids = append(ids, runID)
	}
	sort.Strings(ids)

	var taken []string
	for _, runID := range ids {
		l := t.leases[runID]
		if !l.Expired(nowMs) || l.Owner == newOwner {
			continue
		}
		l.Owner = newOwner
		l.Generation++
		l.AcquiredMs = nowMs
		l.ExpiresAtMs = nowMs + ttlMs
		t.leases[runID] = l
		t.history[runID] = append(t.history[runID], l.Generation)
		taken = append(taken, runID)
	}
	return taken
}

// IsOwner reports whether owner currently holds a valid lease on a run.
func (t *LeaseTable) IsOwner(runID, owner string, nowMs int64) bool {
	l, ok := t.leases[runID]
	return ok && l.Owner == owner && !l.Expired(nowMs)
}

// Get returns a run's lease.
func (t *LeaseTable) Get(runID string) (Lease, bool) {
	l, ok := t.leases[runID]
	return l, ok
}

// OwnedWithGenerations returns the unexpired leases an owner holds.
func (t *LeaseTable) OwnedWithGenerations(owner string, nowMs int64) map[string]int64 {
	out := map[string]int64{}
	for runID, l := range t.leases {
		if l.Owner == owner && !l.Expired(nowMs) {
			out[runID] = l.Generation
		}
	}
	return out
}

// Release drops a lease, as a graceful handoff does.
func (t *LeaseTable) Release(runID, owner string) bool {
	l, ok := t.leases[runID]
	if !ok || l.Owner != owner {
		return false
	}
	delete(t.leases, runID)
	// A graceful handoff deletes the row, so the next acquisition genuinely
	// starts a NEW generation chain at 1. Clearing the history says that
	// explicitly; leaving it would make the oracle read the restart as a
	// generation that went backwards.
	delete(t.history, runID)
	return true
}

// CheckLeaseSafety is the DT-OWNER-01 oracle.
//
//   - At most one node holds a valid lease on a run at any instant. This is
//     structural here (one row per run), so what is actually checked is that no
//     generation was ever reused or skipped backwards.
//   - Generations strictly increase, by exactly one per takeover. A jump would
//     mean a takeover the table never recorded; a repeat would mean two owners
//     could present the same fence.
//
// It deliberately does NOT check that the previous owner has stopped executing.
// Nothing in a lease can establish that, and a test that assumed it would be
// asserting a guarantee the product does not make.
func (t *LeaseTable) CheckLeaseSafety() error {
	for runID, gens := range t.history {
		if len(gens) == 0 {
			continue
		}
		if gens[0] != 1 {
			return fmt.Errorf("model: run %s first generation is %d, want 1", runID, gens[0])
		}
		for i := 1; i < len(gens); i++ {
			if gens[i] != gens[i-1]+1 {
				return fmt.Errorf("model: run %s generation went %d -> %d, want +1", runID, gens[i-1], gens[i])
			}
		}
		if cur, ok := t.leases[runID]; ok && cur.Generation != gens[len(gens)-1] {
			return fmt.Errorf("model: run %s holds generation %d but history ends at %d",
				runID, cur.Generation, gens[len(gens)-1])
		}
	}
	return nil
}
