package receipt

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// makeReceipt finalizes a receipt from raw entries so the digest is populated.
func makeReceipt(git, manifest string, tasks []TaskEntry) *Receipt {
	r := &Receipt{
		RunID:               uuid.New(),
		JobID:               uuid.New(),
		GitCommit:           git,
		ManifestContentHash: manifest,
		Tasks:               tasks,
	}
	r.finalize()
	return r
}

func pinnedEntry(name, hash, digest string) TaskEntry {
	e := TaskEntry{TaskName: name, IdentityHash: hash, Image: name + ":1", ResolvedImageDigest: digest}
	markDegraded(&e)
	return e
}

// TestComputeDigestStableAcrossOrder: finalize sorts, so two receipts built
// from the same entries in different orders share a digest.
func TestComputeDigestStableAcrossOrder(t *testing.T) {
	a := makeReceipt("g", "m", []TaskEntry{
		pinnedEntry("b", "hb", "sha256:b"),
		pinnedEntry("a", "ha", "sha256:a"),
	})
	b := makeReceipt("g", "m", []TaskEntry{
		pinnedEntry("a", "ha", "sha256:a"),
		pinnedEntry("b", "hb", "sha256:b"),
	})
	require.Equal(t, a.ReceiptDigest, b.ReceiptDigest)
}

// TestComputeDigestSensitiveToDigest: changing a resolved digest changes the
// receipt digest (the whole point of folding the digest in).
func TestComputeDigestSensitiveToDigest(t *testing.T) {
	a := makeReceipt("g", "m", []TaskEntry{pinnedEntry("a", "ha", "sha256:a")})
	b := makeReceipt("g", "m", []TaskEntry{pinnedEntry("a", "ha", "sha256:DIFFERENT")})
	require.NotEqual(t, a.ReceiptDigest, b.ReceiptDigest)
}

// TestComputeDigestSensitiveToManifestAndGit: manifest and git commit are both
// folded into the digest.
func TestComputeDigestSensitiveToManifestAndGit(t *testing.T) {
	base := makeReceipt("g1", "m1", []TaskEntry{pinnedEntry("a", "ha", "sha256:a")})
	diffManifest := makeReceipt("g1", "m2", []TaskEntry{pinnedEntry("a", "ha", "sha256:a")})
	diffGit := makeReceipt("g2", "m1", []TaskEntry{pinnedEntry("a", "ha", "sha256:a")})

	require.NotEqual(t, base.ReceiptDigest, diffManifest.ReceiptDigest)
	require.NotEqual(t, base.ReceiptDigest, diffGit.ReceiptDigest)
}

// TestDiffTaskAddedAndMissing: a task added in one receipt and missing from the
// other is reported in both directions.
func TestDiffTaskAddedAndMissing(t *testing.T) {
	committed := makeReceipt("g", "m", []TaskEntry{
		pinnedEntry("a", "ha", "sha256:a"),
		pinnedEntry("b", "hb", "sha256:b"),
	})
	rederived := makeReceipt("g", "m", []TaskEntry{
		pinnedEntry("a", "ha", "sha256:a"),
		pinnedEntry("c", "hc", "sha256:c"),
	})

	drifts := diff(committed, rederived)

	require.True(t, hasDrift(drifts, DriftTaskMissing, "b"), "b dropped → missing")
	require.True(t, hasDrift(drifts, DriftTaskAdded, "c"), "c appeared → added")
	require.True(t, hasDrift(drifts, DriftReceiptDigest, ""))
}

// TestDiffVersionMismatch: receipts of different schema versions are flagged as
// not directly comparable.
func TestDiffVersionMismatch(t *testing.T) {
	committed := makeReceipt("g", "m", []TaskEntry{pinnedEntry("a", "ha", "sha256:a")})
	committed.ReceiptVersion = Version + 1 // pretend it came from a newer builder

	rederived := makeReceipt("g", "m", []TaskEntry{pinnedEntry("a", "ha", "sha256:a")})

	drifts := diff(committed, rederived)
	require.True(t, hasDrift(drifts, DriftVersion, ""))
}

// TestDiffCleanNoDrift: identical receipts produce no drift.
func TestDiffCleanNoDrift(t *testing.T) {
	a := makeReceipt("g", "m", []TaskEntry{
		pinnedEntry("a", "ha", "sha256:a"),
		pinnedEntry("b", "hb", "sha256:b"),
	})
	b := makeReceipt("g", "m", []TaskEntry{
		pinnedEntry("a", "ha", "sha256:a"),
		pinnedEntry("b", "hb", "sha256:b"),
	})
	require.Empty(t, diff(a, b))
}

// TestMarkDegradedClassification covers the three branches of markDegraded.
func TestMarkDegradedClassification(t *testing.T) {
	pinned := TaskEntry{TaskName: "ok", IdentityHash: "h", Image: "x:1", ResolvedImageDigest: "sha256:x"}
	markDegraded(&pinned)
	require.True(t, pinned.DigestPinned)
	require.False(t, pinned.Degraded)

	unpinned := TaskEntry{TaskName: "u", IdentityHash: "h", Image: "x:latest"}
	markDegraded(&unpinned)
	require.False(t, unpinned.DigestPinned)
	require.True(t, unpinned.Degraded)
	require.Contains(t, unpinned.DegradedReason, "not digest-pinned")

	noHash := TaskEntry{TaskName: "n", IdentityHash: "", Image: "x:1", ResolvedImageDigest: "sha256:x"}
	markDegraded(&noHash)
	require.True(t, noHash.Degraded)
	require.Contains(t, noHash.DegradedReason, "no identity hash")
}

func hasDrift(drifts []Drift, kind DriftKind, task string) bool {
	for _, d := range drifts {
		if d.Kind == kind && d.Task == task {
			return true
		}
	}
	return false
}
