package task

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validFingerprint = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestParseMarkers_StringFormArray(t *testing.T) {
	logs := strings.NewReader(`##caesium::partitions ["2026-07-01","2026-07-02"]
`)
	got, err := ParseMarkers(logs)
	require.NoError(t, err)
	require.Equal(t, []Partition{
		{Key: "2026-07-01"},
		{Key: "2026-07-02"},
	}, got.Partitions)
}

func TestParseMarkers_LineForm(t *testing.T) {
	logs := strings.NewReader(`##caesium::partition a
##caesium::partition b
##caesium::partition a
`)
	got, err := ParseMarkers(logs)
	require.NoError(t, err)
	require.Equal(t, []Partition{{Key: "a"}, {Key: "b"}}, got.Partitions)
}

func TestParseMarkers_ObjectAndMixed(t *testing.T) {
	logs := strings.NewReader(fmt.Sprintf(`##caesium::partitions ["a", {"key":"b","fingerprint":%q,"dependsOn":["a"],"root":"stacks/b"}]
`, validFingerprint))
	got, err := ParseMarkers(logs)
	require.NoError(t, err)
	require.Len(t, got.Partitions, 2)
	assert.Equal(t, "a", got.Partitions[0].Key)
	assert.Empty(t, got.Partitions[0].Fingerprint)
	assert.Equal(t, "b", got.Partitions[1].Key)
	assert.Equal(t, validFingerprint, got.Partitions[1].Fingerprint)
	assert.Equal(t, []string{"a"}, got.Partitions[1].DependsOn)
	assert.Equal(t, map[string]string{"root": "stacks/b"}, got.Partitions[1].Attributes)
}

func TestParseMarkers_AppendedArrays(t *testing.T) {
	logs := strings.NewReader(`##caesium::partitions ["a"]
##caesium::partitions ["b","c"]
`)
	got, err := ParseMarkers(logs)
	require.NoError(t, err)
	require.Equal(t, []Partition{{Key: "a"}, {Key: "b"}, {Key: "c"}}, got.Partitions)
}

func TestParseMarkers_IdenticalDuplicateDedups(t *testing.T) {
	logs := strings.NewReader(`##caesium::partitions ["a","a"]
##caesium::partition a
`)
	got, err := ParseMarkers(logs)
	require.NoError(t, err)
	require.Equal(t, []Partition{{Key: "a"}}, got.Partitions)
}

func TestParseMarkers_ConflictingDuplicateFails(t *testing.T) {
	logs := strings.NewReader(fmt.Sprintf(`##caesium::partitions [{"key":"a","fingerprint":%q},{"key":"a","root":"other"}]
`, validFingerprint))
	_, err := ParseMarkers(logs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"a"`)
	assert.Contains(t, err.Error(), "conflicting")
}

func TestParseMarkers_MalformedFingerprintFails(t *testing.T) {
	logs := strings.NewReader(`##caesium::partitions [{"key":"a","fingerprint":"not-a-digest"}]
`)
	_, err := ParseMarkers(logs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"a"`)
	assert.Contains(t, err.Error(), "fingerprint")
}

func TestParseMarkers_NonScalarAttributeFails(t *testing.T) {
	logs := strings.NewReader(`##caesium::partitions [{"key":"a","meta":{"nested":true}}]
`)
	_, err := ParseMarkers(logs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"a"`)
	assert.Contains(t, err.Error(), "meta")
}

func TestParseMarkers_NeitherStringNorObjectFails(t *testing.T) {
	logs := strings.NewReader(`##caesium::partitions [1]
`)
	_, err := ParseMarkers(logs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "neither a string nor an object")
}

func TestParseMarkers_EmptyKeyFails(t *testing.T) {
	logs := strings.NewReader(`##caesium::partitions [""]
`)
	_, err := ParseMarkers(logs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestParseMarkers_KeyTooLongFails(t *testing.T) {
	key := strings.Repeat("k", MaxPartitionKeyBytes+1)
	logs := strings.NewReader(fmt.Sprintf(`##caesium::partitions [%q]
`, key))
	_, err := ParseMarkers(logs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")
}

func TestParseMarkers_CountCapFails(t *testing.T) {
	logs := strings.NewReader(`##caesium::partitions ["a","b","c"]
`)
	_, err := ParseMarkersWithLimits(logs, 0, 2)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "count cap")
	assert.Contains(t, err.Error(), `"c"`)
}

func TestParseMarkers_ObjectByteCapFails(t *testing.T) {
	attrs := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		attrs = append(attrs, fmt.Sprintf(`"a%d":"%s"`, i, strings.Repeat("x", 300)))
	}
	logs := strings.NewReader(fmt.Sprintf(`##caesium::partitions [{"key":"big",%s}]
`, strings.Join(attrs, ",")))
	_, err := ParseMarkers(logs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"big"`)
	assert.Contains(t, err.Error(), "object cap")
}

func TestParseMarkers_AttributeCountCapFails(t *testing.T) {
	attrs := make([]string, 0, MaxPartitionAttributes+1)
	for i := 0; i < MaxPartitionAttributes+1; i++ {
		attrs = append(attrs, fmt.Sprintf(`"k%d":"v"`, i))
	}
	logs := strings.NewReader(fmt.Sprintf(`##caesium::partitions [{"key":"a",%s}]
`, strings.Join(attrs, ",")))
	_, err := ParseMarkers(logs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"a"`)
	assert.Contains(t, err.Error(), "attributes")
}

func TestParseMarkers_ListByteCapFails(t *testing.T) {
	// 1024 keys of ~200B each exceed 256KB normalized.
	parts := make([]string, 0, 1024)
	for i := 0; i < 1024; i++ {
		parts = append(parts, fmt.Sprintf("%q", fmt.Sprintf("k%04d-%s", i, strings.Repeat("z", 240))))
	}
	logs := strings.NewReader(fmt.Sprintf("##caesium::partitions [%s]\n", strings.Join(parts, ",")))
	_, err := ParseMarkersWithLimits(logs, 0, 1024)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "byte limit")
}

func TestParseMarkers_CountCapBoundary(t *testing.T) {
	logs := strings.NewReader(`##caesium::partitions ["a","b"]
`)
	got, err := ParseMarkersWithLimits(logs, 0, 2)
	require.NoError(t, err)
	require.Len(t, got.Partitions, 2)
}

func TestPartitionCanonicalJSON_StringForm(t *testing.T) {
	p := Partition{Key: "a"}
	raw, err := p.CanonicalJSON()
	require.NoError(t, err)
	assert.JSONEq(t, `{"key":"a"}`, string(raw))
}

func TestParseMarkers_1025ExceedsDefaultCap(t *testing.T) {
	parts := make([]string, 0, 1025)
	for i := 0; i < 1025; i++ {
		parts = append(parts, fmt.Sprintf(`"p%d"`, i))
	}
	logs := strings.NewReader(fmt.Sprintf("##caesium::partitions [%s]\n", strings.Join(parts, ",")))
	_, err := ParseMarkers(logs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "count cap")
	assert.Contains(t, err.Error(), `"p1024"`)
}

// TestParseMarkers_DependsOnNormalizedForPersistence pins the fix for the
// adversarial-review finding that dependsOn was normalized only inside
// ValidatePartitionGraph's local loop variable. internal/run/fanout.go marshals
// p.DependsOn verbatim into task_runs.partition_depends_on, so " a " validated
// as an edge to "a" and was then persisted with the whitespace intact — the
// stored edge never matched any sibling's partition_value. The parser must hand
// back the canonical form so the graph and the persisted rows use one key set.
func TestParseMarkers_DependsOnNormalizedForPersistence(t *testing.T) {
	logs := strings.NewReader(`##caesium::partitions ["a",{"key":"b","dependsOn":["  a  "]}]
`)
	got, err := ParseMarkers(logs)
	require.NoError(t, err)
	require.Len(t, got.Partitions, 2)
	assert.Equal(t, []string{"a"}, got.Partitions[1].DependsOn,
		"dependsOn must be trimmed by the parser so persistence stores the same key the graph resolved")

	// The encoded form is what fanout.go writes to partition_depends_on.
	encoded, err := EncodePartitions(got.Partitions)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), `"  a  "`)

	g, err := ValidatePartitionGraph(got.Partitions)
	require.NoError(t, err)
	assert.Equal(t, 1, g.Indegree["b"])
	assert.Equal(t, []string{"b"}, g.Dependents["a"])
}

// TestParseMarkers_DependsOnDedupedForPersistence: the graph counts a repeated
// dep once (indegree 1). Persistence must agree, or the stored edge list and the
// seeded outstanding_predecessors describe different graphs.
func TestParseMarkers_DependsOnDedupedForPersistence(t *testing.T) {
	logs := strings.NewReader(`##caesium::partitions ["a",{"key":"b","dependsOn":["a"," a ","a\t"]}]
`)
	got, err := ParseMarkers(logs)
	require.NoError(t, err)
	require.Len(t, got.Partitions, 2)
	assert.Equal(t, []string{"a"}, got.Partitions[1].DependsOn)

	g, err := ValidatePartitionGraph(got.Partitions)
	require.NoError(t, err)
	assert.Equal(t, 1, g.Indegree["b"])
	assert.Equal(t, []string{"b"}, g.Dependents["a"])
}

// TestParseMarkers_DependsOnWhitespaceOnlyRejected: a dep that is empty after
// trimming is a producer bug, not an edge to drop silently.
func TestParseMarkers_DependsOnWhitespaceOnlyRejected(t *testing.T) {
	logs := strings.NewReader(`##caesium::partitions ["a",{"key":"b","dependsOn":["   "]}]
`)
	_, err := ParseMarkers(logs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dependsOn")
	assert.Contains(t, err.Error(), "empty")
}

// TestParseMarkers_DependsOnWhitespaceVariantsDedupToOnePartition: the key set
// is trimmed at parse time, so two emissions whose keys differ only in
// surrounding whitespace are the same partition. Their dependsOn lists must
// canonicalize identically too, or the identical-payload dedup would report a
// spurious conflict.
func TestParseMarkers_DependsOnWhitespaceVariantsDedupToOnePartition(t *testing.T) {
	logs := strings.NewReader(`##caesium::partitions ["a",{"key":"b","dependsOn":["a"]},{"key":" b ","dependsOn":[" a "]}]
`)
	got, err := ParseMarkers(logs)
	require.NoError(t, err)
	require.Equal(t, []Partition{{Key: "a"}, {Key: "b", DependsOn: []string{"a"}}}, got.Partitions)
}

// TestParseMarkers_NullPartitionsArrayRejected pins the fix for the
// adversarial-review finding that `##caesium::partitions null` unmarshalled into
// a nil []json.RawMessage without error, so a producer that emitted a null work
// list was silently treated as "emitted nothing" and routed through onEmpty
// (skip by default) instead of failing the producer.
func TestParseMarkers_NullPartitionsArrayRejected(t *testing.T) {
	logs := strings.NewReader(`##caesium::partitions null
`)
	_, err := ParseMarkers(logs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "null")
}

// TestParseMarkers_EmptyPartitionsArrayStillValid: `[]` is the documented way to
// emit an empty work list and must keep routing through onEmpty. It must also
// come back as a NON-NIL empty slice, not nil: cache.Entry.Partitions and
// run.Store.HasFanOutSuccessor's cache-hit gate rely on nil-vs-non-nil-empty to
// tell "recorded, and it was empty" apart from "the marker was never emitted at
// all" — collapsing the two here would make a legitimately-empty producer
// permanently un-cacheable (F7 P1-1).
func TestParseMarkers_EmptyPartitionsArrayStillValid(t *testing.T) {
	logs := strings.NewReader(`##caesium::partitions []
`)
	got, err := ParseMarkers(logs)
	require.NoError(t, err)
	assert.Empty(t, got.Partitions)
	require.NotNil(t, got.Partitions, "an explicitly-emitted empty array must round-trip as a non-nil empty slice, not nil")
}

// TestParseMarkers_NoPartitionsMarkerStaysNil pins the OTHER half of the same
// invariant: a log stream that never mentions partitions at all (not a fan-out
// producer, or a producer that forgot the marker) must yield a NIL slice, not an
// empty one — the only way this signal can reach cache.Entry.Partitions and the
// F7 cache-hit gate correctly.
func TestParseMarkers_NoPartitionsMarkerStaysNil(t *testing.T) {
	logs := strings.NewReader("ordinary output\nno markers here\n")
	got, err := ParseMarkers(logs)
	require.NoError(t, err)
	require.Nil(t, got.Partitions)
}

// TestParseMarkers_NonArrayPartitionsRejected: any non-array top-level token is
// a producer bug and must fail the task rather than parse as "no partitions".
func TestParseMarkers_NonArrayPartitionsRejected(t *testing.T) {
	for name, payload := range map[string]string{
		"object": `{"key":"a"}`,
		"string": `"a"`,
		"number": `7`,
		"bool":   `true`,
	} {
		t.Run(name, func(t *testing.T) {
			logs := strings.NewReader("##caesium::partitions " + payload + "\n")
			_, err := ParseMarkers(logs)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "JSON array")
		})
	}
}

// TestNormalizePartitions is the entry point for callers that obtain a partition
// list from somewhere other than the marker parser (a recovery path rebuilding
// from persisted rows, a test fixture). It must apply the identical dependsOn
// rules, and must not mutate the caller's slice.
func TestNormalizePartitions(t *testing.T) {
	in := []Partition{
		{Key: "a"},
		{Key: "b", DependsOn: []string{" a ", "a"}},
	}
	got, err := NormalizePartitions(in)
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, got[1].DependsOn)
	assert.Equal(t, []string{" a ", "a"}, in[1].DependsOn, "the caller's slice must not be mutated")

	// Idempotent.
	again, err := NormalizePartitions(got)
	require.NoError(t, err)
	assert.Equal(t, got, again)
}

// TestNormalizePartitions_EmptyDepRejected mirrors the parser's rejection so a
// non-parser source cannot smuggle an unresolvable edge into persistence.
func TestNormalizePartitions_EmptyDepRejected(t *testing.T) {
	_, err := NormalizePartitions([]Partition{{Key: "b", DependsOn: []string{"\t"}}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dependsOn")
}
