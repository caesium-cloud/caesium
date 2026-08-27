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
