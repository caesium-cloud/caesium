package run

import (
	"strconv"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/cache"
)

// whydiff_chain_test.go covers the read side of `cache.chain: values`
// (infra-deploy A4). Spec §4.3: `caesium why` MUST render the exclusion
// explicitly, "or the skip becomes unexplainable" — a task reported `cached`
// while its predecessor's identity visibly moved reads as a cache bug until the
// diff says the predecessor hashes were never part of the key.

// chainInput is a `mid` step consuming one upstream output, parameterized by the
// upstream's own identity hash and by the value it published.
func chainInput(chain, predHash, predOutput string) cache.HashInput {
	return cache.HashInput{
		JobAlias:           "chain-job",
		TaskName:           "mid",
		Image:              "alpine:3.23",
		Command:            []string{"sh", "-c", "true"},
		PredecessorHashes:  []string{predHash},
		PredecessorOutputs: map[string]map[string]string{"upstream": {"token": predOutput}},
		CacheVersion:       1,
		Chain:              chain,
	}
}

// TestDiff_ValuesChainReportsExclusionNotAChange is the load-bearing case: two
// values-mode blobs whose predecessor hashes differ but whose keys are equal.
// The diff must report a cache hit with the exclusion named, NOT a
// predecessorHashes add/remove pair that never discriminated anything.
func TestDiff_ValuesChainReportsExclusionNotAChange(t *testing.T) {
	baseline := blobFor(t, chainInput(cache.ChainValues, "upstream-hash-run-1", "same"))
	subject := blobFor(t, chainInput(cache.ChainValues, "upstream-hash-run-2", "same"))

	diff, err := DiffHashInputBlobs(subject, baseline)
	if err != nil {
		t.Fatalf("DiffHashInputBlobs: %v", err)
	}
	if !diff.HashEqual {
		t.Fatalf("values-mode keys must be equal across a predecessor re-run: %+v", diff)
	}

	if len(diff.Notes) != 1 || diff.Notes[0] != "predecessor hashes excluded (chain: values)" {
		t.Fatalf("expected the exclusion note, got %+v", diff.Notes)
	}

	c, ok := findChange(diff.Changes, "predecessorHashes")
	if !ok {
		t.Fatalf("expected a predecessorHashes entry naming the exclusion, got %+v", diff.Changes)
	}
	if c.Kind != fieldExcluded {
		t.Fatalf("predecessorHashes must be reported as excluded, got kind %q", c.Kind)
	}
	if c.Note != "excluded (chain: values)" {
		t.Fatalf("unexpected note %q", c.Note)
	}
	if c.Added || c.Removed || c.Before != "" || c.After != "" {
		t.Fatalf("an excluded entry must not claim a before/after change: %+v", c)
	}

	if got := discriminatingChanges(diff.Changes); len(got) != 0 {
		t.Fatalf("an excluded entry must not count as a discriminating field: %+v", got)
	}
}

// TestDiff_ValuesChainStillNamesChangedOutputs: the exclusion is additive, not a
// blanket suppression. A changed predecessor OUTPUT is still the headline.
func TestDiff_ValuesChainStillNamesChangedOutputs(t *testing.T) {
	baseline := blobFor(t, chainInput(cache.ChainValues, "upstream-hash-run-1", "v1"))
	subject := blobFor(t, chainInput(cache.ChainValues, "upstream-hash-run-2", "v2"))

	diff, err := DiffHashInputBlobs(subject, baseline)
	if err != nil {
		t.Fatalf("DiffHashInputBlobs: %v", err)
	}
	if diff.HashEqual {
		t.Fatal("a changed predecessor output must still change a values-mode key")
	}

	c, ok := findChange(diff.Changes, "predecessorOutputs.upstream.token")
	if !ok {
		t.Fatalf("expected the changed output to be named, got %+v", diff.Changes)
	}
	if c.Before != "v1" || c.After != "v2" {
		t.Fatalf("unexpected before/after: %+v", c)
	}
	if got := discriminatingChanges(diff.Changes); len(got) != 1 {
		t.Fatalf("expected exactly one discriminating field, got %+v", got)
	}
	if len(diff.Notes) != 1 {
		t.Fatalf("the exclusion note must still be present: %+v", diff.Notes)
	}
}

// TestDiff_TransitiveChainUnchanged is the no-regression guard: a transitive
// pair still reports the predecessor-hash set change and carries no notes, so
// every existing explanation renders byte-identically.
func TestDiff_TransitiveChainUnchanged(t *testing.T) {
	baseline := blobFor(t, chainInput(cache.ChainTransitive, "upstream-hash-run-1", "same"))
	subject := blobFor(t, chainInput("", "upstream-hash-run-2", "same"))

	diff, err := DiffHashInputBlobs(subject, baseline)
	if err != nil {
		t.Fatalf("DiffHashInputBlobs: %v", err)
	}
	if diff.HashEqual {
		t.Fatal("a transitive consumer must still cascade from a changed predecessor hash")
	}
	if len(diff.Notes) != 0 {
		t.Fatalf("transitive diffs must carry no chain notes, got %+v", diff.Notes)
	}

	c, ok := findChange(diff.Changes, "predecessorHashes")
	if !ok {
		t.Fatalf("expected the predecessor-hash change, got %+v", diff.Changes)
	}
	if c.Kind == fieldExcluded {
		t.Fatal("a transitive predecessor-hash change must be reported as a real change")
	}
}

// TestDiff_ChainModeSwitchIsExplained: when only one side is values-mode the two
// keys genuinely are not comparable on this input, so the exclusion is still the
// honest thing to report rather than a phantom add/remove.
func TestDiff_ChainModeSwitchIsExplained(t *testing.T) {
	baseline := blobFor(t, chainInput(cache.ChainTransitive, "upstream-hash-run-1", "same"))
	subject := blobFor(t, chainInput(cache.ChainValues, "upstream-hash-run-1", "same"))

	diff, err := DiffHashInputBlobs(subject, baseline)
	if err != nil {
		t.Fatalf("DiffHashInputBlobs: %v", err)
	}
	if diff.HashEqual {
		t.Fatal("the chain mode is part of the identity, so a switch must change the key")
	}
	if len(diff.Notes) != 1 {
		t.Fatalf("a one-sided values mode must still be explained: %+v", diff.Notes)
	}
	c, ok := findChange(diff.Changes, "predecessorHashes")
	if !ok || c.Kind != fieldExcluded {
		t.Fatalf("expected the exclusion marker, got %+v", diff.Changes)
	}

	// The mode itself must be reported as a REAL discriminating field. The
	// exclusion marker above is filtered out of every count, so without a `chain`
	// entry the miss has no attributable cause and summarizeChanged falls through
	// to "cause is outside the persisted hash inputs" — which is false here: the
	// cause is the blob's own chain field.
	mode, ok := findChange(diff.Changes, "chain")
	if !ok {
		t.Fatalf("expected the chain mode switch to be named, got %+v", diff.Changes)
	}
	if mode.Kind != fieldScalar {
		t.Fatalf("the chain switch is a scalar change, got kind %q", mode.Kind)
	}
	if mode.Before != cache.ChainTransitive || mode.After != cache.ChainValues {
		t.Fatalf("chain change must read transitive→values, got %q→%q", mode.Before, mode.After)
	}

	got := discriminatingChanges(diff.Changes)
	if len(got) != 1 || got[0].Field != "chain" {
		t.Fatalf("the mode switch must be the discriminating field, got %+v", got)
	}
}

// TestDiff_ChainOmittedMeansTransitive: the blob omits `chain` in the default
// mode (that omission is what keeps pre-chain blobs byte-identical), so an
// unset-vs-"transitive" pair must NOT be reported as a mode switch.
func TestDiff_ChainOmittedMeansTransitive(t *testing.T) {
	baseline := blobFor(t, chainInput("", "upstream-hash-run-1", "same"))
	subject := blobFor(t, chainInput(cache.ChainTransitive, "upstream-hash-run-1", "same"))

	diff, err := DiffHashInputBlobs(subject, baseline)
	if err != nil {
		t.Fatalf("DiffHashInputBlobs: %v", err)
	}
	if !diff.HashEqual {
		t.Fatal("an unset chain must hash identically to an explicit transitive")
	}
	if _, ok := findChange(diff.Changes, "chain"); ok {
		t.Fatalf("an omitted chain must not read as a mode change, got %+v", diff.Changes)
	}
}

// TestSummarize_ChainModeSwitchIsNotMisattributed is the user-visible half of
// the fix: the one-line summary a mode switch produces must name the mode, and
// must NOT claim the cause lies outside the persisted hash inputs.
func TestSummarize_ChainModeSwitchIsNotMisattributed(t *testing.T) {
	baseline := blobFor(t, chainInput(cache.ChainTransitive, "upstream-hash-run-1", "same"))
	subject := blobFor(t, chainInput(cache.ChainValues, "upstream-hash-run-1", "same"))

	diff, err := DiffHashInputBlobs(subject, baseline)
	if err != nil {
		t.Fatalf("DiffHashInputBlobs: %v", err)
	}

	exp := &WhyExplanation{
		TaskName: "plan",
		Verdict:  VerdictCacheMiss,
		Status:   string(TaskStatusSucceeded),
		Baseline: WhyBaseline{Kind: "prior_run"},
		Diff:     diff,
	}
	summary := summarize(exp)

	if strings.Contains(summary, "outside the persisted hash inputs") {
		t.Fatalf("a mode switch IS inside the persisted hash inputs; summary: %q", summary)
	}
	if !strings.Contains(summary, "`chain` changed transitive→values") {
		t.Fatalf("the summary must name the mode switch; got %q", summary)
	}
	if !strings.Contains(summary, predecessorHashesExcludedNote) {
		t.Fatalf("the summary must still carry the exclusion note; got %q", summary)
	}
}

// TestDiff_OversizedValuesBlobStillNamesExclusion: the field-level detail is
// gone, but "predecessor hashes were not part of this key" is exactly the fact a
// degraded reader most needs — PredecessorCount would otherwise read as "N
// inputs that entered the key".
func TestDiff_OversizedValuesBlobStillNamesExclusion(t *testing.T) {
	oversized := func(predHash string) []byte {
		in := chainInput(cache.ChainValues, predHash, "same")
		in.Env = make(map[string]string, 4096)
		for i := 0; i < 4096; i++ {
			in.Env["VAR_"+strconv.Itoa(i)] = strings.Repeat("x", 64)
		}
		return blobFor(t, in)
	}

	diff, err := DiffHashInputBlobs(oversized("run-2"), oversized("run-1"))
	if err != nil {
		t.Fatalf("DiffHashInputBlobs: %v", err)
	}
	if diff.Degraded == "" {
		t.Fatal("this fixture must exceed the blob bound")
	}
	if len(diff.Notes) != 1 || !strings.Contains(diff.Notes[0], "chain: values") {
		t.Fatalf("a degraded values-mode diff must still name the exclusion: %+v", diff.Notes)
	}
}

// TestDiff_ValuesSubjectWithMissingBaselineStillNamesExclusion pins greptile
// 3881714472. Upgrading an existing task to `chain: values` is precisely the
// case that lands on the missing-blob early return: the first run after the
// upgrade has a values-mode subject blob and a baseline that is absent (no prior
// run recorded a blob, or the prior blob predates A2). The note used to be
// attached only after that return, so the reader was told the detail was
// degraded and never that predecessor hashes were deliberately not part of the
// key — the single fact that explains why the identity moved.
func TestDiff_ValuesSubjectWithMissingBaselineStillNamesExclusion(t *testing.T) {
	subject := blobFor(t, chainInput(cache.ChainValues, "upstream-hash-run-2", "same"))

	diff, err := DiffHashInputBlobs(subject, nil)
	if err != nil {
		t.Fatalf("DiffHashInputBlobs: %v", err)
	}
	if diff.Degraded == "" {
		t.Fatal("a missing baseline must still report the degradation")
	}
	if diff.SubjectHash == "" {
		t.Fatal("the subject side parsed, so its hash must be surfaced")
	}
	if len(diff.Notes) != 1 || diff.Notes[0] != predecessorHashesExcludedNote {
		t.Fatalf("a values-mode subject with no baseline must still name the exclusion: %+v", diff.Notes)
	}

	// And it reaches the one line an operator actually reads.
	exp := &WhyExplanation{
		TaskName: "plan",
		Verdict:  VerdictCacheMiss,
		Status:   string(TaskStatusSucceeded),
		Baseline: WhyBaseline{Kind: "prior_run"},
		Diff:     diff,
	}
	if summary := summarize(exp); !strings.Contains(summary, predecessorHashesExcludedNote) {
		t.Fatalf("the summary must carry the exclusion note on the degraded path; got %q", summary)
	}
}

// TestDiff_ValuesBaselineWithMissingSubjectStillNamesExclusion is the mirror
// side: the cache-origin entry was written in values mode but this run recorded
// no blob (caching turned off for the run, say). The exclusion still explains
// what the surviving hash means.
func TestDiff_ValuesBaselineWithMissingSubjectStillNamesExclusion(t *testing.T) {
	baseline := blobFor(t, chainInput(cache.ChainValues, "upstream-hash-run-1", "same"))

	diff, err := DiffHashInputBlobs(nil, baseline)
	if err != nil {
		t.Fatalf("DiffHashInputBlobs: %v", err)
	}
	if diff.BaselineHash == "" {
		t.Fatal("the baseline side parsed, so its hash must be surfaced")
	}
	if len(diff.Notes) != 1 || diff.Notes[0] != predecessorHashesExcludedNote {
		t.Fatalf("a values-mode baseline with no subject must still name the exclusion: %+v", diff.Notes)
	}
}

// TestDiff_TransitiveSubjectWithMissingBaselineHasNoExclusionNote guards the
// other direction: the note is a values-mode fact, so decoding the surviving
// side must not start attaching it to ordinary transitive explanations.
func TestDiff_TransitiveSubjectWithMissingBaselineHasNoExclusionNote(t *testing.T) {
	subject := blobFor(t, chainInput(cache.ChainTransitive, "upstream-hash-run-2", "same"))

	diff, err := DiffHashInputBlobs(subject, nil)
	if err != nil {
		t.Fatalf("DiffHashInputBlobs: %v", err)
	}
	if len(diff.Notes) != 0 {
		t.Fatalf("a transitive diff must carry no chain notes: %+v", diff.Notes)
	}
}

// TestDiff_BothBlobsMissingHasNoExclusionNote: nothing decoded, nothing to say
// beyond the degradation itself.
func TestDiff_BothBlobsMissingHasNoExclusionNote(t *testing.T) {
	diff, err := DiffHashInputBlobs(nil, nil)
	if err != nil {
		t.Fatalf("DiffHashInputBlobs: %v", err)
	}
	if diff.Degraded == "" {
		t.Fatal("two missing blobs must report the degradation")
	}
	if len(diff.Notes) != 0 {
		t.Fatalf("no blob decoded, so no chain note is knowable: %+v", diff.Notes)
	}
}

// TestDiff_UndecodableSurvivingBlobIsNotFatal: the early return decodes
// best-effort, so a corrupt surviving blob degrades quietly rather than
// erroring, exactly as it did before the note was moved.
func TestDiff_UndecodableSurvivingBlobIsNotFatal(t *testing.T) {
	diff, err := DiffHashInputBlobs([]byte("{not json"), nil)
	if err != nil {
		t.Fatalf("a corrupt surviving blob must not error: %v", err)
	}
	if diff.SubjectHash != "" || len(diff.Notes) != 0 {
		t.Fatalf("nothing is knowable from a corrupt blob: %+v", diff)
	}
}
