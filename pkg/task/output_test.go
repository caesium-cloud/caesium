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
