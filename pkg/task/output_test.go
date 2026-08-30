package task

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustBuildOutputEnv(t *testing.T, predOutputs map[string]map[string]string) map[string]string {
	t.Helper()
	env, err := BuildOutputEnv(predOutputs)
	require.NoError(t, err)
	return env
}

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

func TestParseOutput_NonScalarValues_Dropped(t *testing.T) {
	// JSON null, objects, and arrays are not meaningful scalar output values.
	// They must be dropped (no entry), never coerced to "<nil>" / "map[...]" /
	// "[...]" — that garbage would flow into CAESIUM_OUTPUT_* env vars,
	// outputSchema validation, lineage, and the freshness watermark store.
	logs := strings.NewReader(`##caesium::output {"nul": null, "obj": {"x": 1}, "arr": [1, 2], "ok": "kept"}
`)
	result, err := ParseOutput(logs)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"ok": "kept"}, result)
	assert.NotContains(t, result, "nul")
	assert.NotContains(t, result, "obj")
	assert.NotContains(t, result, "arr")
}

func TestParseMarkers_NonScalarValues_Dropped(t *testing.T) {
	logs := strings.NewReader(`##caesium::output {"nul": null, "obj": {"x": 1}, "arr": [1, 2], "ok": "kept"}
`)
	m, err := ParseMarkers(logs)
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, "kept", m.Output["ok"])
	assert.NotContains(t, m.Output, "nul")
	assert.NotContains(t, m.Output, "obj")
	assert.NotContains(t, m.Output, "arr")
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
	env := mustBuildOutputEnv(t, predOutputs)
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
	assert.Nil(t, mustBuildOutputEnv(t, nil))
	assert.Nil(t, mustBuildOutputEnv(t, map[string]map[string]string{}))
}

func TestBuildOutputEnv_SinglePredecessor(t *testing.T) {
	predOutputs := map[string]map[string]string{
		"etl-extract": {
			"row_count": "42",
			"path":      "/data/out.parquet",
		},
	}
	env := mustBuildOutputEnv(t, predOutputs)
	assert.Equal(t, "42", env["CAESIUM_OUTPUT_ETL_EXTRACT_ROW_COUNT"])
	assert.Equal(t, "/data/out.parquet", env["CAESIUM_OUTPUT_ETL_EXTRACT_PATH"])
}

func TestBuildOutputEnv_MultiplePredecessors(t *testing.T) {
	predOutputs := map[string]map[string]string{
		"step-a": {"count": "10"},
		"step-b": {"count": "20", "status": "ok"},
	}
	env := mustBuildOutputEnv(t, predOutputs)
	assert.Equal(t, "10", env["CAESIUM_OUTPUT_STEP_A_COUNT"])
	assert.Equal(t, "20", env["CAESIUM_OUTPUT_STEP_B_COUNT"])
	assert.Equal(t, "ok", env["CAESIUM_OUTPUT_STEP_B_STATUS"])
}

func TestBuildOutputEnv_KeyNormalization(t *testing.T) {
	predOutputs := map[string]map[string]string{
		"step-one": {"some-key": "val", "dot.key": "val2"},
	}
	env := mustBuildOutputEnv(t, predOutputs)
	assert.Equal(t, "val", env["CAESIUM_OUTPUT_STEP_ONE_SOME_KEY"])
	assert.Equal(t, "val2", env["CAESIUM_OUTPUT_STEP_ONE_DOT_KEY"])
	// Dashed and dotted keys do not survive lowercasing the folded suffix, so
	// the name index has to carry the originals.
	var index map[string]string
	require.NoError(t, json.Unmarshal([]byte(env["CAESIUM_OUTPUT_STEP_ONE_CAESIUM_OUTPUT_NAMES"]), &index))
	assert.Equal(t, map[string]string{
		"SOME_KEY": "some-key",
		"DOT_KEY":  "dot.key",
	}, index)
}

func TestEncodeOutputNamesIndex_OmitsIdentityMaps(t *testing.T) {
	encoded, err := EncodeOutputNamesIndex(map[string]string{
		"row_count": "42",
		"path":      "/data/out.parquet",
	})
	require.NoError(t, err)
	assert.Empty(t, encoded, "snake_case keys already survive the fold; emitting an index would churn cache keys")
}

func TestEncodeOutputNamesIndex_MapsFoldedKeys(t *testing.T) {
	encoded, err := EncodeOutputNamesIndex(map[string]string{
		"vpcId":                "vpc-1",
		"db-url":               "postgres://db",
		"dot.key":              "v",
		"greeting":             "hello",
		"caesium_output_names": `{"stale":"index"}`,
	})
	require.NoError(t, err)
	var index map[string]string
	require.NoError(t, json.Unmarshal([]byte(encoded), &index))
	assert.Equal(t, map[string]string{
		"VPCID":    "vpcId",
		"DB_URL":   "db-url",
		"DOT_KEY":  "dot.key",
		"GREETING": "greeting",
	}, index)
	assert.NotContains(t, index, "CAESIUM_OUTPUT_NAMES", "the index must not describe itself")
}

func TestBuildOutputEnv_OutputNamesIndexRoundTrip(t *testing.T) {
	predOutputs := map[string]map[string]string{
		"apply-network": {
			"vpcId":    "vpc-1",
			"db-url":   "postgres://db",
			"dot.key":  "v",
			"greeting": "hello",
		},
		"apply-account": {
			"account_id": "acct-1",
		},
	}
	env := mustBuildOutputEnv(t, predOutputs)

	assert.Equal(t, "vpc-1", env["CAESIUM_OUTPUT_APPLY_NETWORK_VPCID"])
	assert.Equal(t, "postgres://db", env["CAESIUM_OUTPUT_APPLY_NETWORK_DB_URL"])
	assert.Equal(t, "v", env["CAESIUM_OUTPUT_APPLY_NETWORK_DOT_KEY"])
	assert.Equal(t, "hello", env["CAESIUM_OUTPUT_APPLY_NETWORK_GREETING"])
	assert.Equal(t, "acct-1", env["CAESIUM_OUTPUT_APPLY_ACCOUNT_ACCOUNT_ID"])

	var index map[string]string
	require.NoError(t, json.Unmarshal([]byte(env["CAESIUM_OUTPUT_APPLY_NETWORK_CAESIUM_OUTPUT_NAMES"]), &index))
	assert.Equal(t, "vpcId", index["VPCID"])
	assert.Equal(t, "db-url", index["DB_URL"])
	assert.Equal(t, "dot.key", index["DOT_KEY"])
	assert.Equal(t, "greeting", index["GREETING"])

	_, hasAccountIndex := env["CAESIUM_OUTPUT_APPLY_ACCOUNT_CAESIUM_OUTPUT_NAMES"]
	assert.False(t, hasAccountIndex, "an identity map must not grow an extra env var (cache key churn)")
}

func TestBuildOutputEnv_ReplacesStoredNameIndex(t *testing.T) {
	env := mustBuildOutputEnv(t, map[string]map[string]string{
		"apply-network": {
			"vpcId":                     "vpc-1",
			OutputNamesIndexKey:         `{"VPCID":"stale"}`,
			"caesium_outputs_published": "1",
		},
	})
	assert.Equal(t, "vpc-1", env["CAESIUM_OUTPUT_APPLY_NETWORK_VPCID"])
	var index map[string]string
	require.NoError(t, json.Unmarshal([]byte(env["CAESIUM_OUTPUT_APPLY_NETWORK_CAESIUM_OUTPUT_NAMES"]), &index))
	assert.Equal(t, "vpcId", index["VPCID"])
	assert.Equal(t, "1", env["CAESIUM_OUTPUT_APPLY_NETWORK_CAESIUM_OUTPUTS_PUBLISHED"])
	assert.NotContains(t, index, "CAESIUM_OUTPUT_NAMES")
}

func TestParseOutput_ReservedOutputNamesScalarFails(t *testing.T) {
	_, err := ParseOutput(strings.NewReader(`##caesium::output {"caesium_output_names":"hello"}` + "\n"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrReservedOutputNamesKey)
	assert.Contains(t, err.Error(), OutputNamesIndexKey)
}

func TestParseMarkers_ReservedOutputNamesScalarFails(t *testing.T) {
	_, err := ParseMarkers(strings.NewReader(`##caesium::output {"caesium_output_names":"hello","ok":"kept"}` + "\n"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrReservedOutputNamesKey)
	assert.Contains(t, err.Error(), OutputNamesIndexKey)
}

func TestParseOutput_ReservedOutputNamesIndexStringAccepted(t *testing.T) {
	logs := strings.NewReader(`##caesium::output {"vpcId":"vpc-1","caesium_output_names":"{\"VPCID\":\"vpcId\"}"}` + "\n")
	result, err := ParseOutput(logs)
	require.NoError(t, err)
	assert.Equal(t, "vpc-1", result["vpcId"])
	index, ok := DecodeOutputNamesIndex(result[OutputNamesIndexKey])
	require.True(t, ok)
	assert.Equal(t, "vpcId", index["VPCID"])
}

func TestParseOutput_ReservedOutputNamesIndexObjectAccepted(t *testing.T) {
	logs := strings.NewReader(`##caesium::output {"vpcId":"vpc-1","caesium_output_names":{"VPCID":"vpcId"}}` + "\n")
	result, err := ParseOutput(logs)
	require.NoError(t, err)
	assert.Equal(t, "vpc-1", result["vpcId"])
	index, ok := DecodeOutputNamesIndex(result[OutputNamesIndexKey])
	require.True(t, ok)
	assert.Equal(t, "vpcId", index["VPCID"])
}

func TestParseOutput_ReservedOutputNamesNonIndexObjectFails(t *testing.T) {
	_, err := ParseOutput(strings.NewReader(`##caesium::output {"caesium_output_names":{"VPCID":1}}` + "\n"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrReservedOutputNamesKey)
}

func TestParseOutput_ReservedOutputNamesRefFails(t *testing.T) {
	line := `##caesium::output-ref {"key":"caesium_output_names","path":"/p","digest":"` + testDigest + `"}` + "\n"
	_, err := ParseOutput(strings.NewReader(line))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrReservedOutputNamesKey)
}

func TestBuildOutputEnv_ForwardsNonIndexReservedKeyWhenNoGeneratedIndex(t *testing.T) {
	env := mustBuildOutputEnv(t, map[string]map[string]string{
		"extract": {
			"row_count":         "42",
			OutputNamesIndexKey: "hello",
		},
	})
	assert.Equal(t, "42", env["CAESIUM_OUTPUT_EXTRACT_ROW_COUNT"])
	assert.Equal(t, "hello", env["CAESIUM_OUTPUT_EXTRACT_CAESIUM_OUTPUT_NAMES"],
		"a non-index user value must not be omitted when no sidecar is generated")
}

func TestBuildOutputEnv_RefusesToOverwriteNonIndexReservedKey(t *testing.T) {
	_, err := BuildOutputEnv(map[string]map[string]string{
		"apply-network": {
			"vpcId":             "vpc-1",
			OutputNamesIndexKey: "hello",
		},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrReservedOutputNamesKey)
	assert.Contains(t, err.Error(), "apply-network")
	assert.Contains(t, err.Error(), OutputNamesIndexKey)
}

func TestBuildOutputEnv_SnakeCaseOmitsSidecar(t *testing.T) {
	env := mustBuildOutputEnv(t, map[string]map[string]string{
		"extract": {"row_count": "42", "path": "/data/out.parquet"},
	})
	_, hasIndex := env["CAESIUM_OUTPUT_EXTRACT_CAESIUM_OUTPUT_NAMES"]
	assert.False(t, hasIndex, "snake_case-only maps must omit the sidecar so cache keys stay stable")
}

func TestIsOutputNamesIndex(t *testing.T) {
	assert.True(t, IsOutputNamesIndex(`{"VPCID":"vpcId"}`))
	assert.True(t, IsOutputNamesIndex(`{}`))
	assert.False(t, IsOutputNamesIndex("hello"))
	assert.False(t, IsOutputNamesIndex(`["vpcId"]`))
	assert.False(t, IsOutputNamesIndex(`{"VPCID":1}`))
	assert.False(t, IsOutputNamesIndex("null"))
	assert.False(t, IsOutputNamesIndex(""))
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

// TestAggregateFanInOutputs_UnderCap is the unchanged happy path: every scalar
// key folds into a JSON object keyed by partition value, alongside the synthetic
// counters.
func TestAggregateFanInOutputs_UnderCap(t *testing.T) {
	got, err := AggregateFanInOutputs("process", map[string]map[string]string{
		"a": {"rows": "1"},
		"b": {"rows": "2"},
	}, 2, 0)
	require.NoError(t, err)
	assert.Equal(t, `{"a":"1","b":"2"}`, got["rows"])
	assert.Equal(t, "2", got["PARTITION_COUNT"])
	assert.Equal(t, "2", got["SUCCEEDED"])
	assert.Equal(t, "0", got["FAILED"])
}

// TestAggregateFanInOutputs_EmptyGroup: a group with no per-partition outputs
// still publishes the counters, and that is not an error.
func TestAggregateFanInOutputs_EmptyGroup(t *testing.T) {
	got, err := AggregateFanInOutputs("process", nil, 0, 3)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"PARTITION_COUNT": "3",
		"SUCCEEDED":       "0",
		"FAILED":          "3",
	}, got)
}

// TestAggregateFanInOutputs_OversizedReturnsError pins the fix for the
// adversarial-review finding that an over-cap aggregate silently dropped EVERY
// user key and returned counters only. A downstream consumer reading
// CAESIUM_OUTPUT_<PRODUCER>_<KEY> would see the variable simply vanish — a
// structural change to the data contract reported as success, and one that also
// slipped past the producer's declared outputSchema. The group must fail loudly
// instead.
func TestAggregateFanInOutputs_OversizedReturnsError(t *testing.T) {
	byPartition := make(map[string]map[string]string, 64)
	blob := strings.Repeat("x", 2048)
	for i := 0; i < 64; i++ {
		byPartition[fmt.Sprintf("p%03d", i)] = map[string]string{"payload": blob}
	}

	got, err := AggregateFanInOutputs("process", byPartition, 64, 0)
	require.Error(t, err)
	assert.Nil(t, got, "no partial aggregate may be returned alongside the error")

	var tooLarge *FanInAggregateTooLargeError
	require.ErrorAs(t, err, &tooLarge)
	require.ErrorIs(t, err, ErrFanInAggregateTooLarge)
	assert.Equal(t, "process", tooLarge.Producer)
	assert.Equal(t, MaxOutputBytes, tooLarge.Cap)
	assert.Greater(t, tooLarge.Size, MaxOutputBytes)
	assert.Contains(t, err.Error(), "process")
}

// TestAggregateFanInOutputs_JustUnderCapKeepsUserKeys guards the boundary: an
// aggregate that fits must keep every user key, not degrade to counters.
func TestAggregateFanInOutputs_JustUnderCapKeepsUserKeys(t *testing.T) {
	byPartition := map[string]map[string]string{
		"only": {"payload": strings.Repeat("x", MaxOutputBytes/2)},
	}
	got, err := AggregateFanInOutputs("process", byPartition, 1, 0)
	require.NoError(t, err)
	assert.Contains(t, got, "payload")
	assert.Equal(t, "1", got["PARTITION_COUNT"])
}
