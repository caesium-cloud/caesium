// Package receipt builds and verifies content-addressed, git-committable
// reproducibility receipts for a job run.
//
// A receipt is a Merkle-style aggregation over a run's persisted identity:
//
//	receiptDigest = sha256(
//	    sorted per-task identity hashes +
//	    resolved image digests +
//	    manifest content hash +
//	    git commit )
//
// It is the REPRODUCE half of the data-plane-memory substrate (see
// docs/design-data-plane-memory.md): a small, deterministic, commit-into-git
// artifact that attests *what ran*. `caesium verify` re-derives the receipt
// from the run's persisted state and flags DRIFT — e.g. a `:latest` tag that
// moved to new content (digest mismatch) or a changed manifest. It does NOT
// resurrect deleted source data; it re-derives the signature and proves what
// the system recorded as having run.
//
// # Correctness rule (from the design)
//
// A receipt over an UNPINNED, mutable tag is unsound: nothing in the recorded
// identity is tamper-evident against a tag that moves underneath. So when a
// task's ResolvedImageDigest is empty — digest pinning was off, or a
// Podman/k8s path fell back to the literal tag — that task is marked
// DigestPinned=false / Degraded=true, and the receipt as a whole is marked
// Degraded. The receipt digest is still computed (it folds in the literal tag,
// the only identity available), but verify reports honestly that the run is
// NOT fully reproducible. We never silently attest a mutable tag as
// reproducible.
package receipt

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// Version is the schema version of the receipt format. Bump it whenever the
// canonical serialization or the digest derivation changes, so a verifier can
// refuse to compare receipts produced by incompatible builders rather than
// reporting spurious drift.
const Version = 1

// TaskEntry is one task's contribution to the receipt: its identity hash (the
// cache key that decided whether it ran or was served from cache) plus the
// image identity that hash covers. Entries are sorted by TaskName before the
// receipt digest folds them in, so the digest is independent of run-order.
type TaskEntry struct {
	// TaskName is the stable, human-meaningful name of the task within the job.
	// It is the sort key for deterministic aggregation.
	TaskName string `json:"task_name"`

	// IdentityHash is the persisted TaskRun.Hash — the content-addressed cache
	// key computed by internal/cache.HashInput.Compute. Empty when the task
	// never had a hash computed (caching disabled for it); such a task cannot
	// be attested and forces Degraded.
	IdentityHash string `json:"identity_hash"`

	// Image is the literal image reference (tag) the task ran with, recorded
	// for human readability and as the fallback identity when no digest was
	// resolved.
	Image string `json:"image"`

	// ResolvedImageDigest is the content digest (sha256:...) the tag resolved
	// to when digest pinning was on. Empty when pinning was off or a
	// Podman/k8s path fell back to the tag.
	ResolvedImageDigest string `json:"resolved_image_digest,omitempty"`

	// DigestPinned is true iff ResolvedImageDigest is non-empty — i.e. the
	// image identity is tamper-evident for this task.
	DigestPinned bool `json:"digest_pinned"`

	// Degraded is true when this task cannot be soundly attested as
	// reproducible: either its image was not digest-pinned (mutable tag) or it
	// has no identity hash at all. Such a task is honestly surfaced rather than
	// silently attested.
	Degraded bool `json:"degraded"`

	// DegradedReason is a short human-readable explanation set when Degraded is
	// true (e.g. "image not digest-pinned: mutable tag 'myimage:latest'").
	DegradedReason string `json:"degraded_reason,omitempty"`
}

// Receipt is the full, content-addressed reproducibility receipt for one run.
// It is JSON-serializable and intended to be committed to git alongside the
// pipeline that produced it. ReceiptDigest is the single value `caesium verify`
// re-derives and compares.
type Receipt struct {
	// ReceiptVersion is Version at build time.
	ReceiptVersion int `json:"receipt_version"`

	// RunID, JobID and JobAlias identify the run this receipt attests. They are
	// metadata for humans and lookups; they are NOT folded into ReceiptDigest
	// (two runs of byte-identical inputs produce the same digest, which is the
	// point — the digest addresses *what ran*, not *which run instance*).
	RunID    uuid.UUID `json:"run_id"`
	JobID    uuid.UUID `json:"job_id"`
	JobAlias string    `json:"job_alias,omitempty"`

	// GitCommit is the provenance commit SHA the manifest was applied from
	// (empty for non-GitOps applies). Folded into ReceiptDigest.
	GitCommit string `json:"git_commit,omitempty"`

	// ManifestContentHash is the content hash of the DAG topology the run
	// executed (from the matching dag_snapshot). Folded into ReceiptDigest so a
	// changed manifest yields a changed receipt.
	ManifestContentHash string `json:"manifest_content_hash,omitempty"`

	// Tasks are the per-task entries, sorted by TaskName. Folded into
	// ReceiptDigest in that order.
	Tasks []TaskEntry `json:"tasks"`

	// Degraded is true iff any task is Degraded — i.e. the run is NOT fully
	// reproducible (at least one mutable, unpinned tag, or a task with no
	// identity hash). When true, callers must not present this receipt as a
	// reproducibility guarantee.
	Degraded bool `json:"degraded"`

	// DegradedTasks lists the names of every degraded task, for a concise
	// honest summary.
	DegradedTasks []string `json:"degraded_tasks,omitempty"`

	// ReceiptDigest is the sha256 hex Merkle aggregate over the canonical
	// per-task lines + manifest content hash + git commit. It is the receipt's
	// content address and the value `verify` re-derives.
	ReceiptDigest string `json:"receipt_digest"`
}

// canonicalTaskLine renders a single task entry to the exact byte sequence
// folded into the receipt digest. The format is stable and unambiguous: every
// field is length-delimited by a separator that cannot appear in the values
// (task names, hashes, and digests do not contain newlines or the \x00 unit
// separator). The "degraded" bit is included so that an unpinned task and a
// pinned one with the same tag can never collide on the same digest.
func canonicalTaskLine(t TaskEntry) string {
	return fmt.Sprintf("task\x00%s\x00hash\x00%s\x00image\x00%s\x00digest\x00%s\x00pinned\x00%t\n",
		t.TaskName, t.IdentityHash, t.Image, t.ResolvedImageDigest, t.DigestPinned)
}

// computeDigest derives the receipt digest from already-sorted task entries
// plus the manifest content hash and git commit. It is the single source of
// truth for the aggregation, used by both Build and re-derivation in Verify so
// the two can never diverge.
//
// Entries MUST be sorted by TaskName before calling (Build guarantees this).
func computeDigest(tasks []TaskEntry, manifestContentHash, gitCommit string) string {
	h := sha256.New()
	// Domain-separated, versioned preamble so receipts of different schema
	// versions never share a digest space.
	writeHashf(h, "caesium-receipt\x00v%d\n", Version)
	writeHashf(h, "git_commit\x00%s\n", gitCommit)
	writeHashf(h, "manifest\x00%s\n", manifestContentHash)
	writeHashf(h, "task_count\x00%d\n", len(tasks))
	for _, t := range tasks {
		_, _ = h.Write([]byte(canonicalTaskLine(t)))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeHashf writes a formatted line to a hash.Hash. hash.Hash.Write never
// returns an error, so the result is intentionally discarded (mirrors the w()
// helper in internal/cache/hash.go).
func writeHashf(h hash.Hash, format string, args ...any) {
	_, _ = fmt.Fprintf(h, format, args...)
}

// sortTasks orders entries by TaskName, then by IdentityHash as a tiebreaker
// (a single task name should not repeat in a run, but the tiebreaker keeps the
// aggregation total and deterministic even if it somehow does). It sorts in
// place and returns the slice for chaining.
func sortTasks(tasks []TaskEntry) []TaskEntry {
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].TaskName != tasks[j].TaskName {
			return tasks[i].TaskName < tasks[j].TaskName
		}
		return tasks[i].IdentityHash < tasks[j].IdentityHash
	})
	return tasks
}

// finalize sorts the tasks, derives the degraded summary, and computes the
// receipt digest, mutating r in place. Both Build and the test helpers call it
// so a Receipt is never returned with a stale digest.
func (r *Receipt) finalize() {
	r.ReceiptVersion = Version
	sortTasks(r.Tasks)

	degraded := make([]string, 0)
	for i := range r.Tasks {
		if r.Tasks[i].Degraded {
			degraded = append(degraded, r.Tasks[i].TaskName)
		}
	}
	sort.Strings(degraded)
	r.DegradedTasks = degraded
	r.Degraded = len(degraded) > 0

	r.ReceiptDigest = computeDigest(r.Tasks, r.ManifestContentHash, r.GitCommit)
}

// markDegraded classifies a task entry: it is degraded when it has no identity
// hash (never hashed — caching was off) or when its image was not
// digest-pinned (a mutable tag). The reason string is honest and specific so
// `verify` can tell the operator exactly why the run is not reproducible.
func markDegraded(t *TaskEntry) {
	t.DigestPinned = t.ResolvedImageDigest != ""
	switch {
	case strings.TrimSpace(t.IdentityHash) == "":
		t.Degraded = true
		t.DegradedReason = "no identity hash recorded (caching disabled for this task) — cannot attest"
	case !t.DigestPinned:
		t.Degraded = true
		t.DegradedReason = fmt.Sprintf("image not digest-pinned: mutable tag %q — not verifiable", t.Image)
	default:
		t.Degraded = false
		t.DegradedReason = ""
	}
}
