package model_test

import (
	"testing"

	"github.com/caesium-cloud/caesium/test/model"
	"pgregory.net/rapid"
)

// TestLeaseOwnershipProperties generates competing nodes acquiring, renewing
// and sweeping run leases against a shared clock, and checks DT-OWNER-01.
//
// The clock only moves forward and every node reads the same one: this models
// the no-skew case, which is the case the first real crash scenario runs.
// Skew is a different experiment and needs a real cluster to mean anything, so
// it is named here rather than faked.
func TestLeaseOwnershipProperties(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		leases := model.NewLeaseTable()
		nodes := []string{"node-a", "node-b", "node-c"}
		runs := []string{"run-1", "run-2"}
		now := int64(0)
		const ttl = int64(1_000)

		// ownerOf tracks what the test believes, independently of the table, so
		// a disagreement is a finding rather than a tautology.
		ownerOf := map[string]string{}

		t.Repeat(map[string]func(*rapid.T){
			"": func(t *rapid.T) {
				if err := leases.CheckLeaseSafety(); err != nil {
					t.Fatalf("%v", err)
				}
				for runID, want := range ownerOf {
					valid := 0
					for _, node := range nodes {
						if leases.IsOwner(runID, node, now) {
							valid++
							if node != want {
								t.Fatalf("%s is owned by %s, expected %s", runID, node, want)
							}
						}
					}
					if valid > 1 {
						t.Fatalf("%s has %d valid owners at once", runID, valid)
					}
				}
			},

			"tick": func(t *rapid.T) {
				now += int64(rapid.IntRange(1, 900).Draw(t, "ms"))
			},

			"acquire": func(t *rapid.T) {
				runID := rapid.SampledFrom(runs).Draw(t, "run")
				node := rapid.SampledFrom(nodes).Draw(t, "node")
				gen, fresh := leases.Acquire(runID, node, now, ttl)
				if fresh {
					if gen != 1 {
						t.Fatalf("a fresh lease started at generation %d", gen)
					}
					ownerOf[runID] = node
					return
				}
				// Acquisition is idempotent: an existing lease is never stolen,
				// and the caller learns the generation actually in force. A
				// contender that treated this as success would run a second
				// owner on the same run.
				if held, ok := leases.Get(runID); ok && held.Owner != node && held.Owner != ownerOf[runID] {
					t.Fatalf("acquire moved %s to %s", runID, held.Owner)
				}
			},

			"renew": func(t *rapid.T) {
				runID := rapid.SampledFrom(runs).Draw(t, "run")
				node := rapid.SampledFrom(nodes).Draw(t, "node")
				before, held := leases.Get(runID)
				ok := leases.Renew(runID, node, now+ttl)
				if !held {
					if ok {
						t.Fatalf("renewed a lease that does not exist")
					}
					return
				}
				if ok && before.Owner != node {
					t.Fatalf("%s renewed %s, which %s owns", node, runID, before.Owner)
				}
				if !ok && before.Owner == node {
					t.Fatalf("%s could not renew a lease it owns", node)
				}
			},

			"sweep_expired": func(t *rapid.T) {
				node := rapid.SampledFrom(nodes).Draw(t, "node")
				before := map[string]int64{}
				for _, runID := range runs {
					if l, ok := leases.Get(runID); ok {
						before[runID] = l.Generation
					}
				}
				taken := leases.AcquireExpired(node, now, ttl)
				for _, runID := range taken {
					after, _ := leases.Get(runID)
					if after.Generation != before[runID]+1 {
						t.Fatalf("takeover of %s moved the generation %d -> %d",
							runID, before[runID], after.Generation)
					}
					if after.Owner != node {
						t.Fatalf("takeover of %s left it owned by %s", runID, after.Owner)
					}
					ownerOf[runID] = node
				}
				// Nothing unexpired may move.
				for _, runID := range runs {
					l, ok := leases.Get(runID)
					if !ok {
						continue
					}
					if l.Generation != before[runID] && !containsString(taken, runID) {
						t.Fatalf("%s changed generation without being reported taken", runID)
					}
				}
			},

			"concurrent_sweep": func(t *rapid.T) {
				// Two nodes sweeping the same expired set: the expiry predicate
				// is the compare-and-swap, so the first commit moves the rows
				// out of the expired set and the second must find nothing.
				a := rapid.SampledFrom(nodes).Draw(t, "first")
				b := rapid.SampledFrom(nodes).Draw(t, "second")
				if a == b {
					return
				}
				first := leases.AcquireExpired(a, now, ttl)
				second := leases.AcquireExpired(b, now, ttl)
				for _, runID := range first {
					if containsString(second, runID) {
						t.Fatalf("%s was taken over twice at the same instant", runID)
					}
					ownerOf[runID] = a
				}
				for _, runID := range second {
					ownerOf[runID] = b
				}
			},

			"release": func(t *rapid.T) {
				runID := rapid.SampledFrom(runs).Draw(t, "run")
				node := rapid.SampledFrom(nodes).Draw(t, "node")
				if leases.Release(runID, node) {
					delete(ownerOf, runID)
				}
			},
		})
	})
}

func containsString(haystack []string, want string) bool {
	for _, s := range haystack {
		if s == want {
			return true
		}
	}
	return false
}
