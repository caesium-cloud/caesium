package cache

// PriorEntry is a candidate prior successful execution of a task, used to prove
// a value-verified short-circuit. It carries only the two fields the proof
// needs: the identity Hash that execution committed to, and the Output it
// produced. Both come from a persisted cache Entry for the same (job, task),
// so a match is proof — not a heuristic — that the prior identity produced the
// same bytes the current re-execution did.
type PriorEntry struct {
	// Hash is the identity hash the prior successful execution committed to.
	Hash string
	// Output is the structured output that execution produced. For a
	// large-object reference (pkg/task.OutputRef) the value embeds the content
	// digest, so byte-equality of this map proves payload-content equality.
	Output map[string]string
	// CreatedAt orders candidates so the most recent proven-equal prior wins.
	CreatedAt int64
}

// EquivalentPriorHash implements the value-verified short-circuit (design
// Component 5 / exec-plan item D2).
//
// When a step re-executes because its OWN identity hash changed (a cache MISS —
// e.g. its image or command changed), its NEW identity hash would normally fold
// into every downstream task's PredecessorHashes set and force those downstream
// tasks to re-run too — even if the step produced byte-identical output. That
// cascade is the cost a content scheduler pays for a no-op code change.
//
// EquivalentPriorHash stops that cascade *only when content equality is PROVEN*.
// It searches the step's prior successful executions (same job + task name) for
// one whose persisted Output is byte-identical to the output this run just
// produced. If found, it returns that prior execution's identity hash — the
// proven-equivalent identity — which the caller substitutes for the new hash
// when presenting this step to its downstream consumers. A downstream task whose
// only changed input was this step's identity then sees an UNCHANGED predecessor
// hash, cache-hits, and stays green. The skip is proven by digest equality, not
// inferred.
//
// CRITICAL — cache-correctness invariant. A cache miss must always be safe; a
// FALSE short-circuit (presenting a prior identity for a step whose output
// actually changed) would serve a stale downstream result. So this function
// defaults to re-run on ANY uncertainty and only substitutes when ALL hold:
//
//   - newHash is non-empty (the step has a real identity this run).
//   - newOutput is non-nil — a step that emitted no structured output offers no
//     content to prove equality against; we cannot prove the value is unchanged,
//     so we never short-circuit it.
//   - A prior candidate exists whose Hash is non-empty, differs from newHash
//     (an identical hash is already a cache hit — nothing to short-circuit), and
//     whose Output is byte-identical to newOutput by canonical comparison.
//
// On any failure of these it returns newHash unchanged: the conservative,
// always-safe result is to let the changed identity propagate and re-run
// downstream. The most-recent matching prior wins (candidates are compared by
// CreatedAt) so the substituted identity is the freshest proven-equal one.
func EquivalentPriorHash(newHash string, newOutput map[string]string, priors []PriorEntry) string {
	// Guard 1: no real current identity → nothing to substitute.
	if newHash == "" {
		return newHash
	}
	// Guard 2: no structured output → cannot prove the value is unchanged.
	// A step that emits nothing offers no content address to compare, so a
	// changed identity must propagate (re-run downstream). This is deliberately
	// conservative: silence is not proof of equality.
	if len(newOutput) == 0 {
		return newHash
	}

	var (
		best      string
		bestStamp int64
		found     bool
	)
	for _, p := range priors {
		// Guard 3a: a prior with no committed identity cannot be substituted in.
		if p.Hash == "" {
			continue
		}
		// Guard 3b: an identical hash means the current identity already matches
		// this prior — the normal cache path would hit; nothing to short-circuit.
		if p.Hash == newHash {
			continue
		}
		// The proof: byte-identical output — exactly the bytes HashInput.Compute
		// folds into a downstream cache key (its pred_output line). For a
		// large-object reference those bytes embed the sha256 content digest, so a
		// re-emitted byte-identical payload proves content equality. This is
		// deliberately no weaker than the cache key itself: if the output differs
		// at all (a changed digest, or a changed reference path that the key also
		// hashes), it is not proven equal and we re-run. Conservative by
		// construction — never a false short-circuit.
		if !mapsEqual(newOutput, p.Output) {
			continue
		}
		if !found || p.CreatedAt >= bestStamp {
			best = p.Hash
			bestStamp = p.CreatedAt
			found = true
		}
	}

	if !found {
		return newHash
	}
	return best
}

// mapsEqual reports whether two string→string output maps are equal,
// independent of map iteration order. Value equality IS content equality here:
// a large-object reference value embeds its sha256 content digest, so two maps
// are equal exactly when every key carries byte-identical bytes — the same
// bytes HashInput.Compute folds into a downstream cache key. It is O(N) and
// zero-alloc (no serialization), and a length mismatch or any missing/unequal
// key short-circuits to false, so a changed/added/removed output never proves
// equal.
func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || av != bv {
			return false
		}
	}
	return true
}
