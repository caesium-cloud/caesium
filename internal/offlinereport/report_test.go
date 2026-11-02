package offlinereport

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDiagnosticsFromErrorSplitsYAMLPaths(t *testing.T) {
	err := errors.New("jobs/bad.job.yaml: unknown YAML fields:\n  YAML path metadata.schemaValidaton (line 5): unknown field \"schemaValidaton\"\n  YAML path steps[0].dependsON (line 12): unknown field \"dependsON\"")
	diags := DiagnosticsFromError("jobs/bad.job.yaml", err)
	require.Len(t, diags, 2)
	require.Equal(t, "metadata.schemaValidaton", diags[0].Path)
	require.Equal(t, 5, diags[0].Line)
	require.Equal(t, "jobs/bad.job.yaml", diags[0].File)
	require.Equal(t, `unknown field "schemaValidaton"`, diags[0].Message)
	require.Equal(t, 12, diags[1].Line)
	require.NotContains(t, diags[0].Human(), "{")
}

func TestWriteKeepsEmptyActualAndSkipsNotesOnStderr(t *testing.T) {
	report := &Report{
		Command: "test",
		Summary: Summary{Passed: 0, Failed: 1},
		Diagnostics: []Diagnostic{
			{Severity: SeverityNote, Message: "trigger-cycle lint is file-scoped"},
			{Severity: SeverityError, File: "a.job.yaml", Line: 4, Message: "steps must contain at least one entry"},
		},
		Scenarios: []Scenario{{
			Name:   "mismatch",
			Passed: false,
			Assertions: []Assertion{{
				Name:     "task greet output[code]",
				Passed:   false,
				Expected: "1",
				Actual:   "",
			}},
		}},
	}
	var stdout bytes.Buffer
	require.NoError(t, Write(&stdout, report))
	require.Equal(t, Version, report.Version)

	var decoded Report
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &decoded))
	require.Equal(t, "", decoded.Scenarios[0].Assertions[0].Actual)
	require.Equal(t, "1", decoded.Scenarios[0].Assertions[0].Expected)
	require.False(t, decoded.OK)
	require.Equal(t, 1, decoded.Summary.Errors)

	var stderr bytes.Buffer
	require.NoError(t, WriteHumanDiagnostics(&stderr, report.Diagnostics))
	require.NotContains(t, stderr.String(), "trigger-cycle")
	require.NotContains(t, stderr.String(), stdout.String())
	require.Contains(t, stderr.String(), "a.job.yaml:4")
}
