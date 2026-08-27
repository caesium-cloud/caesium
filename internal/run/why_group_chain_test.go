package run

import (
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/cache"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

// why_group_chain_test.go covers the GROUP-level half of the `chain: values`
// explanation (fix round 1, review finding I-1).
//
// A fanned step addressed without a --partition selector answers with a group
// summary: N identity hashes, N baselines, and therefore no `Diff` at all — so
// there was no channel for the exclusion note, and `summarize` returned
// `summarizeGroup` before the note was ever appended. That is the shape spec
// §5.5's own reference manifest uses (`fanOut` AND `chain: values` on the same
// step), and it is the invocation an operator reaches for first, because
// `--partition` requires already knowing the keys.

// TestWhyGroup_ValuesModeCarriesExclusionNote: an operator who edited one stack
// types `caesium why <run> apply` and gets "2 cached, 1 succeeded". Without the
// note there is nothing saying predecessor hashes were excluded from those two
// keys — precisely the unexplainable skip spec §4.3 forbids.
func TestWhyGroup_ValuesModeCarriesExclusionNote(t *testing.T) {
	rows := []models.TaskRun{
		{Status: string(TaskStatusCached), CacheEnabled: true, CacheHit: true, CacheChain: cache.ChainValues, PartitionValue: "stacks/app-web", PartitionIndex: 0},
		{Status: string(TaskStatusCached), CacheEnabled: true, CacheHit: true, CacheChain: cache.ChainValues, PartitionValue: "stacks/app-api", PartitionIndex: 1},
		{Status: string(TaskStatusSucceeded), CacheEnabled: true, CacheChain: cache.ChainValues, PartitionValue: "stacks/network", PartitionIndex: 2},
	}

	exp := newWhyGroupExplanation(uuid.New(), uuid.New(), uuid.New(), "apply", rows)
	exp.Summary = summarize(exp)

	if exp.Diff != nil {
		t.Fatalf("a group answer still carries no diff; got %+v", exp.Diff)
	}
	if len(exp.Group.Notes) != 1 || exp.Group.Notes[0] != predecessorHashesExcludedNote {
		t.Fatalf("the group must carry the exclusion note, got %+v", exp.Group.Notes)
	}
	if !strings.Contains(exp.Summary, predecessorHashesExcludedNote) {
		t.Fatalf("the group summary line must name the exclusion; got %q", exp.Summary)
	}

	// The pre-existing group content must survive unchanged.
	if !strings.Contains(exp.Summary, "FANNED GROUP") ||
		!strings.Contains(exp.Summary, "--partition <value>") {
		t.Fatalf("the group summary lost its existing content; got %q", exp.Summary)
	}
}

// TestWhyGroup_TransitiveCarriesNoNote is the no-regression half: every existing
// fanned explanation must render exactly as it did before. It also pins that an
// explicitly-"transitive" column — what the scheduler writes now that
// ResolveCacheConfig defaults to the literal — reads the same as the empty
// column every pre-chain row carries.
func TestWhyGroup_TransitiveCarriesNoNote(t *testing.T) {
	rows := []models.TaskRun{
		{Status: string(TaskStatusCached), CacheEnabled: true, CacheHit: true, PartitionValue: "a", PartitionIndex: 0},
		{Status: string(TaskStatusCached), CacheEnabled: true, CacheHit: true, CacheChain: cache.ChainTransitive, PartitionValue: "b", PartitionIndex: 1},
	}

	exp := newWhyGroupExplanation(uuid.New(), uuid.New(), uuid.New(), "process", rows)
	exp.Summary = summarize(exp)

	if len(exp.Group.Notes) != 0 {
		t.Fatalf("a transitive group must carry no chain note, got %+v", exp.Group.Notes)
	}
	if strings.Contains(exp.Summary, "excluded") {
		t.Fatalf("a transitive group summary must be unchanged; got %q", exp.Summary)
	}
}

// TestWhyGroup_NoteIsNotDuplicated guards the de-duplication in
// explanationNotes: a future explanation carrying the note on BOTH channels must
// print it once, not twice.
func TestWhyGroup_NoteIsNotDuplicated(t *testing.T) {
	exp := &WhyExplanation{
		TaskName: "apply",
		Verdict:  VerdictCacheHit,
		Diff:     &BlobDiff{HashEqual: true, Notes: []string{predecessorHashesExcludedNote}},
		Group:    &WhyGroup{PartitionCount: 1, StatusCounts: map[string]int{"cached": 1}, Notes: []string{predecessorHashesExcludedNote}},
	}
	if got := explanationNotes(exp); len(got) != 1 {
		t.Fatalf("the note must be de-duplicated across channels, got %+v", got)
	}
	if n := strings.Count(summarize(exp), predecessorHashesExcludedNote); n != 1 {
		t.Fatalf("the summary printed the note %d times, want 1", n)
	}
}
