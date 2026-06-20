package receipt

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// DriftKind classifies a single way a re-derived receipt diverges from a
// committed one. Each value names a concrete, operator-actionable cause.
type DriftKind string

const (
	// DriftReceiptDigest is the top-level signal: the re-derived receipt digest
	// does not match the committed one. It is always accompanied by one or more
	// of the specific drifts below explaining why.
	DriftReceiptDigest DriftKind = "receipt_digest_mismatch"
	// DriftImageDigest means a task's resolved image digest changed — the tag
	// was mutated to point at new content (e.g. a re-pushed :latest). This is
	// the headline reproducibility failure the receipt exists to catch.
	DriftImageDigest DriftKind = "image_digest_mismatch"
	// DriftIdentityHash means a task's identity (cache-key) hash changed —
	// command, env, predecessor outputs, params, or the image digest differ.
	DriftIdentityHash DriftKind = "identity_hash_mismatch"
	// DriftManifest means the DAG manifest content hash changed — the pipeline
	// topology applied for the run differs from what the committed receipt
	// recorded.
	DriftManifest DriftKind = "manifest_changed"
	// DriftGitCommit means the git provenance commit changed.
	DriftGitCommit DriftKind = "git_commit_changed"
	// DriftTaskMissing means a task present in the committed receipt is absent
	// from the re-derived one.
	DriftTaskMissing DriftKind = "task_missing"
	// DriftTaskAdded means a task present in the re-derived receipt is absent
	// from the committed one.
	DriftTaskAdded DriftKind = "task_added"
	// DriftVersion means the two receipts were produced by different schema
	// versions and are not directly comparable.
	DriftVersion DriftKind = "receipt_version_mismatch"
)

// Drift is one detected divergence between a committed receipt and the
// re-derived one. Task is empty for run-level drifts (manifest, git commit,
// version, receipt digest).
type Drift struct {
	Kind     DriftKind `json:"kind"`
	Task     string    `json:"task,omitempty"`
	Expected string    `json:"expected,omitempty"`
	Actual   string    `json:"actual,omitempty"`
	Detail   string    `json:"detail"`
}

// VerifyResult is the outcome of comparing a committed receipt against the
// run's current persisted state.
type VerifyResult struct {
	// RunID echoes the run that was re-derived.
	RunID uuid.UUID `json:"run_id"`

	// Match is true iff the re-derived receipt digest equals the committed one
	// AND the run is not degraded. A degraded run never reports a clean
	// reproducibility match even when the digests are equal, because an
	// unpinned tag could have moved without changing the (tag-only) digest —
	// the very condition the receipt cannot attest against.
	Match bool `json:"match"`

	// Degraded is true iff the re-derived receipt is degraded (any task ran on
	// an unpinned, mutable tag, or had no identity hash). When true, the
	// receipt cannot soundly attest reproducibility regardless of digest
	// equality; Drifts will include the degraded tasks via the rederived
	// receipt's DegradedTasks.
	Degraded bool `json:"degraded"`

	// DegradedTasks lists the tasks that make the run unverifiable.
	DegradedTasks []string `json:"degraded_tasks,omitempty"`

	// ExpectedDigest is the committed receipt's digest; ActualDigest is the
	// freshly re-derived one.
	ExpectedDigest string `json:"expected_digest"`
	ActualDigest   string `json:"actual_digest"`

	// Drifts enumerates every divergence found, empty when Match is true.
	Drifts []Drift `json:"drifts,omitempty"`

	// Rederived is the receipt Build produced from current state, for callers
	// that want to inspect or re-commit it.
	Rederived *Receipt `json:"rederived"`
}

// Verify re-derives the receipt for the run named by committed.RunID from
// current persisted state and compares it against committed, reporting every
// drift. It is the engine behind `caesium verify`: it proves what ran by
// re-deriving the signature, and it surfaces — never hides — the case where the
// run cannot be soundly attested because a task ran on an unpinned tag.
func Verify(ctx context.Context, db *gorm.DB, committed *Receipt) (*VerifyResult, error) {
	if committed == nil {
		return nil, fmt.Errorf("receipt: nil committed receipt")
	}

	rederived, err := Build(ctx, db, committed.RunID)
	if err != nil {
		return nil, err
	}

	result := &VerifyResult{
		RunID:          committed.RunID,
		Degraded:       rederived.Degraded,
		DegradedTasks:  rederived.DegradedTasks,
		ExpectedDigest: committed.ReceiptDigest,
		ActualDigest:   rederived.ReceiptDigest,
		Rederived:      rederived,
	}

	result.Drifts = diff(committed, rederived)

	// A clean match requires equal digests AND a non-degraded run. The degraded
	// case is the design's correctness rule: an unpinned tag may have moved
	// without changing the tag-only digest, so we must not claim a match.
	result.Match = committed.ReceiptDigest == rederived.ReceiptDigest && !rederived.Degraded

	return result, nil
}

// diff compares a committed receipt against a re-derived one and returns every
// divergence. It is exported-package-internal so build and verify share one
// definition of "what counts as drift," and so tests can exercise it directly
// without a database.
func diff(committed, rederived *Receipt) []Drift {
	var drifts []Drift

	if committed.ReceiptVersion != rederived.ReceiptVersion {
		drifts = append(drifts, Drift{
			Kind:     DriftVersion,
			Expected: fmt.Sprintf("v%d", committed.ReceiptVersion),
			Actual:   fmt.Sprintf("v%d", rederived.ReceiptVersion),
			Detail: fmt.Sprintf("receipt schema versions differ (committed v%d, re-derived v%d); not directly comparable",
				committed.ReceiptVersion, rederived.ReceiptVersion),
		})
	}

	if committed.GitCommit != rederived.GitCommit {
		drifts = append(drifts, Drift{
			Kind:     DriftGitCommit,
			Expected: committed.GitCommit,
			Actual:   rederived.GitCommit,
			Detail:   "git provenance commit changed since the receipt was committed",
		})
	}

	if committed.ManifestContentHash != rederived.ManifestContentHash {
		drifts = append(drifts, Drift{
			Kind:     DriftManifest,
			Expected: committed.ManifestContentHash,
			Actual:   rederived.ManifestContentHash,
			Detail:   "DAG manifest content hash changed: the pipeline topology differs from the committed receipt",
		})
	}

	drifts = append(drifts, diffTasks(committed, rederived)...)

	// The receipt-digest mismatch is the umbrella signal; emit it last so the
	// specific causes above read first.
	if committed.ReceiptDigest != rederived.ReceiptDigest {
		drifts = append(drifts, Drift{
			Kind:     DriftReceiptDigest,
			Expected: committed.ReceiptDigest,
			Actual:   rederived.ReceiptDigest,
			Detail:   "re-derived receipt digest does not match the committed receipt",
		})
	}

	return drifts
}

// diffTasks pairs tasks by name and reports per-task drift: missing, added,
// digest mismatch, and identity-hash mismatch. Both receipts are sorted by
// Build/finalize, but diffTasks builds maps so it is order-independent and
// robust to a hand-edited committed receipt.
func diffTasks(committed, rederived *Receipt) []Drift {
	var drifts []Drift

	committedByName := indexByName(committed.Tasks)
	rederivedByName := indexByName(rederived.Tasks)

	// Stable iteration order over the union of names.
	names := unionNames(committedByName, rederivedByName)

	for _, name := range names {
		c, inC := committedByName[name]
		r, inR := rederivedByName[name]

		switch {
		case inC && !inR:
			drifts = append(drifts, Drift{
				Kind:   DriftTaskMissing,
				Task:   name,
				Detail: "task present in the committed receipt is absent from the current run state",
			})
		case !inC && inR:
			drifts = append(drifts, Drift{
				Kind:   DriftTaskAdded,
				Task:   name,
				Detail: "task present in the current run state is absent from the committed receipt",
			})
		default:
			// Present on both sides — compare identity.
			if c.ResolvedImageDigest != r.ResolvedImageDigest {
				drifts = append(drifts, Drift{
					Kind:     DriftImageDigest,
					Task:     name,
					Expected: digestOrTag(c),
					Actual:   digestOrTag(r),
					Detail:   "image tag mutated: resolved content digest differs from the committed receipt",
				})
			}
			if c.IdentityHash != r.IdentityHash {
				drifts = append(drifts, Drift{
					Kind:     DriftIdentityHash,
					Task:     name,
					Expected: c.IdentityHash,
					Actual:   r.IdentityHash,
					Detail:   "task identity (cache-key) hash changed: an input to this task differs",
				})
			}
		}
	}

	return drifts
}

// indexByName maps task entries by name. When a name repeats (it should not
// within a run), the last entry wins — diffTasks's behavior is undefined for
// duplicate names by design, and Build never produces them.
func indexByName(tasks []TaskEntry) map[string]TaskEntry {
	m := make(map[string]TaskEntry, len(tasks))
	for _, t := range tasks {
		m[t.TaskName] = t
	}
	return m
}

// unionNames returns the sorted union of keys from two task maps so drift
// reporting is deterministic regardless of input order.
func unionNames(a, b map[string]TaskEntry) []string {
	set := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		set[k] = struct{}{}
	}
	for k := range b {
		set[k] = struct{}{}
	}
	names := make([]string, 0, len(set))
	for k := range set {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// digestOrTag returns the resolved digest when present, else the literal tag —
// so a drift report on an unpinned task still shows something meaningful
// (and makes the "was never pinned" case legible).
func digestOrTag(t TaskEntry) string {
	if t.ResolvedImageDigest != "" {
		return t.ResolvedImageDigest
	}
	return t.Image + " (unpinned tag)"
}
