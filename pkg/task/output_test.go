package task

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseOutput_NoMarkers(t *testing.T) {
	logs := strings.NewReader("hello world\nsome log line\n")
	result, err := ParseOutput(logs)
	require.NoError(t, err)
	assert.Nil(t, result)
}

func TestParseOutput_EmptyInput(t *testing.T) {
	logs := strings.NewReader("")
	result, err := ParseOutput(logs)
	require.NoError(t, err)
	assert.Nil(t, result)
}

func TestParseOutput_SingleMarker(t *testing.T) {
	logs := strings.NewReader(`some log output
##caesium::output {"row_count": "42", "path": "/data/out.parquet"}
more logs
`)
	result, err := ParseOutput(logs)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"row_count": "42",
		"path":      "/data/out.parquet",
	}, result)
}

func TestParseOutput_MultipleMarkers_LastWriteWins(t *testing.T) {
	logs := strings.NewReader(`##caesium::output {"status": "partial", "count": "10"}
doing work...
##caesium::output {"status": "complete", "rows": "1000"}
`)
	result, err := ParseOutput(logs)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"status": "complete",
		"count":  "10",
		"rows":   "1000",
	}, result)
}

func TestParseOutput_MixedLogLines(t *testing.T) {
	logs := strings.NewReader(`2026-03-17T10:00:00Z Starting ETL pipeline
2026-03-17T10:00:01Z Processing batch 1 of 5
2026-03-17T10:00:02Z Processing batch 2 of 5
##caesium::output {"batches_processed": "5"}
2026-03-17T10:00:05Z Pipeline complete
`)
	result, err := ParseOutput(logs)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"batches_processed": "5",
	}, result)
}

func TestParseOutput_NonStringValues_CoercedToStrings(t *testing.T) {
	logs := strings.NewReader(`##caesium::output {"count": 42, "ready": true, "ratio": 0.95}
`)
	result, err := ParseOutput(logs)
	require.NoError(t, err)
	assert.Equal(t, "42", result["count"])
	assert.Equal(t, "true", result["ready"])
	assert.Equal(t, "0.95", result["ratio"])
}

func TestParseOutput_MalformedJSON_Skipped(t *testing.T) {
	logs := strings.NewReader(`##caesium::output not valid json
##caesium::output {"valid": "data"}
`)
	result, err := ParseOutput(logs)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"valid": "data"}, result)
}

func TestParseOutput_EmptyPayload_Skipped(t *testing.T) {
	logs := strings.NewReader(`##caesium::output
##caesium::output {"key": "value"}
`)
	result, err := ParseOutput(logs)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"key": "value"}, result)
}

func TestParseOutput_MarkerWithPrefix(t *testing.T) {
	// Docker multiplexed logs may have binary header bytes before the text.
	logs := strings.NewReader("\x01\x00\x00\x00\x00\x00\x00\x2a" + `##caesium::output {"key": "value"}`)
	result, err := ParseOutput(logs)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"key": "value"}, result)
}

func TestParseOutput_SizeLimit(t *testing.T) {
	// Build output that exceeds 64KB using many small entries (to avoid
	// scanner line-length limits).
	var sb strings.Builder
	for i := 0; i < 700; i++ {
		key := fmt.Sprintf("key_%04d", i)
		val := strings.Repeat("x", 100)
		fmt.Fprintf(&sb, "##caesium::output {\"%s\": \"%s\"}\n", key, val)
	}
	logs := strings.NewReader(sb.String())
	_, err := ParseOutput(logs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")
}

// ── Large-object reference output (##caesium::output-ref) ────────────

const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestParseOutput_Reference(t *testing.T) {
	logs := strings.NewReader(`writing payload...
##caesium::output-ref {"key":"frame","path":"/data/out.parquet","digest":"` + testDigest + `","size":734003200}
done
`)
	result, err := ParseOutput(logs)
	require.NoError(t, err)
	require.Contains(t, result, "frame")

	ref, ok := DecodeOutputRef(result["frame"])
	require.True(t, ok, "stored value must decode as a reference")
	assert.Equal(t, "/data/out.parquet", ref.Path)
	assert.Equal(t, testDigest, ref.Digest)
	assert.Equal(t, int64(734003200), ref.Size)
	assert.Equal(t, outputRefVersion, ref.Ref)
}

// A payload far larger than MaxOutputBytes passes through the reference
// protocol: only the small reference line (path + digest) is stored, so the
// 64 KB scalar cap never trips. This is the core D1 acceptance behavior.
func TestParseOutput_ReferenceExceeds64KBPayloadSucceeds(t *testing.T) {
	// size is well over MaxOutputBytes (64 KB); the reference itself is tiny.
	logs := strings.NewReader(`##caesium::output-ref {"key":"big","path":"/data/huge.bin","digest":"` + testDigest + `","size":1073741824}` + "\n")
	result, err := ParseOutput(logs)
	require.NoError(t, err)
	require.Contains(t, result, "big")
	ref, ok := DecodeOutputRef(result["big"])
	require.True(t, ok)
	assert.Equal(t, int64(1073741824), ref.Size)
}

func TestParseOutput_ReferenceMalformedSkipped(t *testing.T) {
	cases := []string{
		`##caesium::output-ref {"path":"/p","digest":"` + testDigest + `"}`,                     // missing key
		`##caesium::output-ref {"key":"k","digest":"` + testDigest + `"}`,                       // missing path
		`##caesium::output-ref {"key":"k","path":"/p","digest":"sha256:short"}`,                 // bad digest
		`##caesium::output-ref {"key":"k","path":"/p","digest":"md5:abc"}`,                      // wrong algo
		`##caesium::output-ref {"key":"k","path":"/p","digest":"` + testDigest + `","size":-1}`, // negative size
		`##caesium::output-ref not json`,                                                        // not JSON
	}
	for _, line := range cases {
		result, err := ParseOutput(strings.NewReader(line + "\n"))
		require.NoError(t, err)
		assert.Nil(t, result, "malformed reference must be skipped: %s", line)
	}
}

// A reference and a scalar marker may both be present; both are collected.
func TestParseMarkers_ReferenceAndScalar(t *testing.T) {
	logs := strings.NewReader(`##caesium::output {"rows":"5"}
##caesium::output-ref {"key":"frame","path":"/data/out.bin","digest":"` + testDigest + `"}
`)
	m, err := ParseMarkers(logs)
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, "5", m.Output["rows"])
	ref, ok := DecodeOutputRef(m.Output["frame"])
	require.True(t, ok)
	assert.Equal(t, "/data/out.bin", ref.Path)
}

// CaptureMarkersWithRefLimit drops a reference whose reported size exceeds the
// operator cap, while leaving an under-cap reference (and scalars) intact.
func TestCaptureMarkersWithRefLimit_RejectsOversizedReference(t *testing.T) {
	logs := strings.NewReader(`##caesium::output-ref {"key":"big","path":"/p","digest":"` + testDigest + `","size":2000}
##caesium::output-ref {"key":"ok","path":"/q","digest":"` + testDigest + `","size":500}
`)
	m, err := CaptureMarkersWithRefLimit(logs, MaxLogSnapshotBytes, 1000)
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.NotContains(t, m.Output, "big", "over-cap reference must be dropped")
	assert.Contains(t, m.Output, "ok", "under-cap reference must be kept")
}

func TestOutputRef_EncodeDecodeRoundTrip(t *testing.T) {
	ref := OutputRef{Ref: outputRefVersion, Path: "/data/x", Digest: testDigest, Size: 42}
	encoded := ref.Encode()
	assert.True(t, IsOutputRef(encoded))
	decoded, ok := DecodeOutputRef(encoded)
	require.True(t, ok)
	assert.Equal(t, ref, decoded)
}

func TestOutputRef_EncodeDeterministic(t *testing.T) {
	ref := OutputRef{Ref: outputRefVersion, Path: "/data/x", Digest: testDigest, Size: 42}
	assert.Equal(t, ref.Encode(), ref.Encode(), "encoding must be byte-stable for cache equality")
}

func TestIsOutputRef_RejectsScalars(t *testing.T) {
	assert.False(t, IsOutputRef("42"))
	assert.False(t, IsOutputRef(`{"path":"/data"}`))          // JSON but not a ref
	assert.False(t, IsOutputRef(`{"caesiumOutputRefish":1}`)) // near-miss sentinel
	assert.True(t, IsOutputRef(`{"caesiumOutputRef":1,"path":"/p","digest":"`+testDigest+`"}`))
}

func TestDecodeOutputRef_RejectsIncomplete(t *testing.T) {
	// Has the sentinel but no digest — not a usable reference.
	_, ok := DecodeOutputRef(`{"caesiumOutputRef":1,"path":"/p"}`)
	assert.False(t, ok)
}

func TestBuildOutputEnv_Reference(t *testing.T) {
	ref := OutputRef{Ref: outputRefVersion, Path: "/data/out.parquet", Digest: testDigest, Size: 100}
	predOutputs := map[string]map[string]string{
		"extract": {"frame": ref.Encode(), "rows": "5"},
	}
	env := BuildOutputEnv(predOutputs)
	// Reference exposes the path (not the raw JSON) plus a _DIGEST companion.
	assert.Equal(t, "/data/out.parquet", env["CAESIUM_OUTPUT_EXTRACT_FRAME"])
	assert.Equal(t, testDigest, env["CAESIUM_OUTPUT_EXTRACT_FRAME_DIGEST"])
	// Scalars in the same map are unaffected.
	assert.Equal(t, "5", env["CAESIUM_OUTPUT_EXTRACT_ROWS"])
}

func TestNormalizeStepName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"etl-extract", "ETL_EXTRACT"},
		{"step.one", "STEP_ONE"},
		{"simple", "SIMPLE"},
		{"multi-hyphen-name", "MULTI_HYPHEN_NAME"},
		{"dots.and-hyphens.mixed", "DOTS_AND_HYPHENS_MIXED"},
		{"ALREADY_UPPER", "ALREADY_UPPER"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			assert.Equal(t, tc.expected, NormalizeStepName(tc.input))
		})
	}
}

func TestBuildOutputEnv_Empty(t *testing.T) {
	assert.Nil(t, BuildOutputEnv(nil))
	assert.Nil(t, BuildOutputEnv(map[string]map[string]string{}))
}

func TestBuildOutputEnv_SinglePredecessor(t *testing.T) {
	predOutputs := map[string]map[string]string{
		"etl-extract": {
			"row_count": "42",
			"path":      "/data/out.parquet",
		},
	}
	env := BuildOutputEnv(predOutputs)
	assert.Equal(t, "42", env["CAESIUM_OUTPUT_ETL_EXTRACT_ROW_COUNT"])
	assert.Equal(t, "/data/out.parquet", env["CAESIUM_OUTPUT_ETL_EXTRACT_PATH"])
}

func TestBuildOutputEnv_MultiplePredecessors(t *testing.T) {
	predOutputs := map[string]map[string]string{
		"step-a": {"count": "10"},
		"step-b": {"count": "20", "status": "ok"},
	}
	env := BuildOutputEnv(predOutputs)
	assert.Equal(t, "10", env["CAESIUM_OUTPUT_STEP_A_COUNT"])
	assert.Equal(t, "20", env["CAESIUM_OUTPUT_STEP_B_COUNT"])
	assert.Equal(t, "ok", env["CAESIUM_OUTPUT_STEP_B_STATUS"])
}

func TestBuildOutputEnv_KeyNormalization(t *testing.T) {
	predOutputs := map[string]map[string]string{
		"step-one": {"some-key": "val", "dot.key": "val2"},
	}
	env := BuildOutputEnv(predOutputs)
	assert.Equal(t, "val", env["CAESIUM_OUTPUT_STEP_ONE_SOME_KEY"])
	assert.Equal(t, "val2", env["CAESIUM_OUTPUT_STEP_ONE_DOT_KEY"])
}

// ── ParseBranches tests ─────────────────────────────────────────────

func TestParseBranches_NoMarkers(t *testing.T) {
	logs := strings.NewReader("hello world\nsome log line\n")
	result, err := ParseBranches(logs)
	require.NoError(t, err)
	assert.Nil(t, result)
}

func TestParseBranches_EmptyInput(t *testing.T) {
	logs := strings.NewReader("")
	result, err := ParseBranches(logs)
	require.NoError(t, err)
	assert.Nil(t, result)
}

func TestParseBranches_SingleBranch(t *testing.T) {
	logs := strings.NewReader("some log\n##caesium::branch full-refresh\nmore logs\n")
	result, err := ParseBranches(logs)
	require.NoError(t, err)
	assert.Equal(t, []string{"full-refresh"}, result)
}

func TestParseBranches_MultipleBranches(t *testing.T) {
	logs := strings.NewReader("##caesium::branch path-a\n##caesium::branch path-b\n")
	result, err := ParseBranches(logs)
	require.NoError(t, err)
	assert.Equal(t, []string{"path-a", "path-b"}, result)
}

func TestParseBranches_Deduplication(t *testing.T) {
	logs := strings.NewReader("##caesium::branch fast-path\n##caesium::branch fast-path\n##caesium::branch slow-path\n")
	result, err := ParseBranches(logs)
	require.NoError(t, err)
	assert.Equal(t, []string{"fast-path", "slow-path"}, result)
}

func TestParseBranches_EmptyName_Skipped(t *testing.T) {
	logs := strings.NewReader("##caesium::branch \n##caesium::branch valid\n")
	result, err := ParseBranches(logs)
	require.NoError(t, err)
	assert.Equal(t, []string{"valid"}, result)
}

func TestParseBranches_MixedWithOutputMarkers(t *testing.T) {
	logs := strings.NewReader(`##caesium::output {"key": "value"}
##caesium::branch selected-path
more logs
`)
	result, err := ParseBranches(logs)
	require.NoError(t, err)
	assert.Equal(t, []string{"selected-path"}, result)
}

func TestParseBranches_DockerMultiplexedPrefix(t *testing.T) {
	logs := strings.NewReader("\x01\x00\x00\x00\x00\x00\x00\x1e" + "##caesium::branch my-step")
	result, err := ParseBranches(logs)
	require.NoError(t, err)
	assert.Equal(t, []string{"my-step"}, result)
}

func TestCaptureMarkers_IncludesRawLogSnapshot(t *testing.T) {
	logs := strings.NewReader("2026-03-21T10:00:00Z starting\n##caesium::output {\"rows\": 42}\n")
	result, err := CaptureMarkers(logs, MaxLogSnapshotBytes)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, map[string]string{"rows": "42"}, result.Output)
	assert.Contains(t, result.LogText, "starting")
	assert.Contains(t, result.LogText, "##caesium::output")
	assert.False(t, result.LogTruncated)
}

func TestCaptureMarkers_TruncatesSnapshot(t *testing.T) {
	logs := strings.NewReader("abcdefghijklmnopqrstuvwxyz")
	result, err := CaptureMarkers(logs, 10)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "abcdefghij", result.LogText)
	assert.True(t, result.LogTruncated)
}

func TestCaptureMarkers_AllowsLargeLinesWithinSnapshotLimit(t *testing.T) {
	line := strings.Repeat("x", 128*1024)
	result, err := CaptureMarkers(strings.NewReader(line+"\n"), MaxLogSnapshotBytes)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, line+"\n", result.LogText)
	assert.False(t, result.LogTruncated)
}
