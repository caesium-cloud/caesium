package cache

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// refValue (the encoded large-object reference helper) is defined in
// hash_test.go in this same package; the short-circuit tests reuse it so the
// bytes compared here match exactly what production stores. These digests feed
// it to prove equality tracks the embedded content digest, not the path.
var (
	scDigestA = "sha256:" + strings.Repeat("a", 64)
	scDigestB = "sha256:" + strings.Repeat("b", 64)
)

func TestEquivalentPriorHash_ProvenEqual_ScalarOutput_ShortCircuits(t *testing.T) {
	// The step re-executed (new identity "h_new") but produced output
	// byte-identical to a prior successful run whose identity was "h_old".
	// The proof must substitute h_old so downstream stays a cache hit.
	out := map[string]string{"row_count": "42"}
	priors := []PriorEntry{
		{Hash: "h_old", Output: map[string]string{"row_count": "42"}, CreatedAt: 100},
	}
	got := EquivalentPriorHash("h_new", out, priors)
	assert.Equal(t, "h_old", got, "byte-identical output must present the prior identity")
}

func TestEquivalentPriorHash_ProvenEqual_ReferenceDigest_ShortCircuits(t *testing.T) {
	// A large-object reference: the encoded value carries the sha256 content
	// digest AND the path, exactly as it folds into the downstream cache key
	// (HashInput.Compute's pred_output line). A byte-identical encoded reference
	// — same digest, same path — is proven value-equal and short-circuits.
	out := map[string]string{"frame": refValue(t, "/data/out.parquet", scDigestA)}
	priors := []PriorEntry{
		{Hash: "h_old", Output: map[string]string{"frame": refValue(t, "/data/out.parquet", scDigestA)}, CreatedAt: 100},
	}
	got := EquivalentPriorHash("h_new", out, priors)
	assert.Equal(t, "h_old", got, "a byte-identical reference (same digest+path) must short-circuit")
}

func TestEquivalentPriorHash_GenuinelyChanged_ReferenceDigest_ReRuns(t *testing.T) {
	// Different content digest -> the value really changed -> NO short-circuit.
	// Serving the prior identity here would be a P0 false short-circuit. The
	// digest is the load-bearing difference (same path, changed bytes).
	out := map[string]string{"frame": refValue(t, "/data/out.parquet", scDigestA)}
	priors := []PriorEntry{
		{Hash: "h_old", Output: map[string]string{"frame": refValue(t, "/data/out.parquet", scDigestB)}, CreatedAt: 100},
	}
	got := EquivalentPriorHash("h_new", out, priors)
	assert.Equal(t, "h_new", got, "a changed content digest must re-run downstream")
}

func TestEquivalentPriorHash_ReferenceDifferentPath_ReRuns(t *testing.T) {
	// Conservatism: the encoded reference includes the path, and the path is
	// part of the downstream cache key today (Compute hashes the whole encoded
	// value). So a same-digest payload written to a DIFFERENT path is NOT
	// byte-identical output and must default to re-run — matching the cache
	// key's own equality, never weaker than it.
	out := map[string]string{"frame": refValue(t, "/data/run-new.parquet", scDigestA)}
	priors := []PriorEntry{
		{Hash: "h_old", Output: map[string]string{"frame": refValue(t, "/data/run-old.parquet", scDigestA)}, CreatedAt: 100},
	}
	got := EquivalentPriorHash("h_new", out, priors)
	assert.Equal(t, "h_new", got, "a changed reference path is not byte-identical output -> re-run (conservative)")
}

func TestEquivalentPriorHash_GenuinelyChanged_ScalarOutput_ReRuns(t *testing.T) {
	out := map[string]string{"row_count": "43"}
	priors := []PriorEntry{
		{Hash: "h_old", Output: map[string]string{"row_count": "42"}, CreatedAt: 100},
	}
	got := EquivalentPriorHash("h_new", out, priors)
	assert.Equal(t, "h_new", got, "changed scalar output must re-run downstream")
}

func TestEquivalentPriorHash_NoPriors_ReRuns(t *testing.T) {
	out := map[string]string{"row_count": "42"}
	got := EquivalentPriorHash("h_new", out, nil)
	assert.Equal(t, "h_new", got, "no prior to prove against must re-run")
}

func TestEquivalentPriorHash_EmptyCurrentOutput_ReRuns(t *testing.T) {
	// A step that emitted no structured output offers no content to prove
	// equality, so a changed identity must propagate (re-run) even if a prior
	// also emitted nothing — silence is not proof.
	priors := []PriorEntry{
		{Hash: "h_old", Output: nil, CreatedAt: 100},
	}
	got := EquivalentPriorHash("h_new", nil, priors)
	assert.Equal(t, "h_new", got, "no current output cannot be proven equal")
}

func TestEquivalentPriorHash_PriorMissingOutput_ReRuns(t *testing.T) {
	// The current run produced output but the only prior emitted none: not
	// provably equal, so re-run.
	out := map[string]string{"row_count": "42"}
	priors := []PriorEntry{
		{Hash: "h_old", Output: nil, CreatedAt: 100},
	}
	got := EquivalentPriorHash("h_new", out, priors)
	assert.Equal(t, "h_new", got, "a prior with no output cannot prove equality")
}

func TestEquivalentPriorHash_PriorMissingHash_ReRuns(t *testing.T) {
	// A prior with a byte-identical output but no committed identity cannot be
	// substituted in — there is no prior hash to present.
	out := map[string]string{"row_count": "42"}
	priors := []PriorEntry{
		{Hash: "", Output: map[string]string{"row_count": "42"}, CreatedAt: 100},
	}
	got := EquivalentPriorHash("h_new", out, priors)
	assert.Equal(t, "h_new", got, "a prior with no identity hash cannot short-circuit")
}

func TestEquivalentPriorHash_EmptyNewHash_ReturnsEmpty(t *testing.T) {
	out := map[string]string{"row_count": "42"}
	priors := []PriorEntry{
		{Hash: "h_old", Output: map[string]string{"row_count": "42"}, CreatedAt: 100},
	}
	got := EquivalentPriorHash("", out, priors)
	assert.Equal(t, "", got, "no current identity -> nothing to substitute")
}

func TestEquivalentPriorHash_IdenticalHash_SkippedAsCandidate(t *testing.T) {
	// A prior whose hash equals the current identity is already a normal cache
	// hit; it must not be treated as a short-circuit candidate. With only that
	// prior, the result stays the (unchanged) current hash.
	out := map[string]string{"row_count": "42"}
	priors := []PriorEntry{
		{Hash: "h_new", Output: map[string]string{"row_count": "42"}, CreatedAt: 100},
	}
	got := EquivalentPriorHash("h_new", out, priors)
	assert.Equal(t, "h_new", got, "a prior identical to the current identity is not a short-circuit")
}

func TestEquivalentPriorHash_MostRecentProvenEqualWins(t *testing.T) {
	// Two distinct prior identities both produced byte-identical output; the
	// most recent (highest CreatedAt) proven-equal identity is presented.
	out := map[string]string{"row_count": "42"}
	priors := []PriorEntry{
		{Hash: "h_older", Output: map[string]string{"row_count": "42"}, CreatedAt: 100},
		{Hash: "h_newer", Output: map[string]string{"row_count": "42"}, CreatedAt: 200},
	}
	got := EquivalentPriorHash("h_new", out, priors)
	assert.Equal(t, "h_newer", got, "the freshest proven-equal prior identity must win")
}

func TestEquivalentPriorHash_MixedCandidates_PicksTheEqualOne(t *testing.T) {
	// A genuinely-different prior (newer) must NOT mask an equal prior (older):
	// only the byte-identical candidate is eligible.
	out := map[string]string{"row_count": "42"}
	priors := []PriorEntry{
		{Hash: "h_equal", Output: map[string]string{"row_count": "42"}, CreatedAt: 100},
		{Hash: "h_diff", Output: map[string]string{"row_count": "99"}, CreatedAt: 300},
	}
	got := EquivalentPriorHash("h_new", out, priors)
	assert.Equal(t, "h_equal", got, "a newer non-matching prior must not block an older proven-equal one")
}

func TestEquivalentPriorHash_OutputKeyOrderIrrelevant(t *testing.T) {
	// Map iteration order must never produce a spurious inequality.
	out := map[string]string{"a": "1", "b": "2", "c": "3"}
	priors := []PriorEntry{
		{Hash: "h_old", Output: map[string]string{"c": "3", "a": "1", "b": "2"}, CreatedAt: 100},
	}
	got := EquivalentPriorHash("h_new", out, priors)
	assert.Equal(t, "h_old", got, "key order must not affect the equality proof")
}

func TestEquivalentPriorHash_PartialKeyMatch_ReRuns(t *testing.T) {
	// Same value for one key but an extra key in the current output is NOT
	// equal — the output changed, so re-run.
	out := map[string]string{"row_count": "42", "checksum": "deadbeef"}
	priors := []PriorEntry{
		{Hash: "h_old", Output: map[string]string{"row_count": "42"}, CreatedAt: 100},
	}
	got := EquivalentPriorHash("h_new", out, priors)
	assert.Equal(t, "h_new", got, "an added output key means the value changed -> re-run")
}
