package localrun

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/stretchr/testify/require"
)

func TestFailureLogTailStaysInsideSnapshotCap(t *testing.T) {
	require.Less(t, failureLogTailBytes, pkgtask.MaxLogSnapshotBytes)
}

func TestFormatRunFailureListsOnlyFailedTasks(t *testing.T) {
	code := 7
	cause := errors.New(`task abc failed with result "unknown"`)
	err := formatRunFailure(cause, &RunResult{Tasks: []TaskResult{
		{Name: "ok", Status: "succeeded", Attempt: 1, LogText: "DX_SHOULD_NOT_LEAK"},
		{
			Name:     "extract",
			Status:   "failed",
			Attempt:  2,
			ExitCode: &code,
			LogText:  "DX_STDOUT_MISSING_COLUMN\nDX_STDERR_CUSTOMER_ID_REQUIRED\n",
		},
	}})
	require.ErrorIs(t, err, cause)
	msg := err.Error()
	require.Contains(t, msg, cause.Error())
	require.Contains(t, msg, "step extract attempt 2 exit 7")
	require.Contains(t, msg, "DX_STDOUT_MISSING_COLUMN")
	require.Contains(t, msg, "DX_STDERR_CUSTOMER_ID_REQUIRED")
	require.NotContains(t, msg, "DX_SHOULD_NOT_LEAK")
	require.NotContains(t, msg, "step ok")
}

func TestFormatRunFailureKeepsCauseWhenNothingFailed(t *testing.T) {
	cause := errors.New("validation: bad")
	require.Equal(t, cause, formatRunFailure(cause, nil))
	require.Equal(t, cause, formatRunFailure(cause, &RunResult{Tasks: []TaskResult{
		{Name: "solo", Status: "succeeded", LogText: "ok"},
	}}))
	require.NoError(t, formatRunFailure(nil, &RunResult{Tasks: []TaskResult{
		{Name: "extract", Status: "failed", LogText: "secret"},
	}}))
}

func TestFormatRunFailureOmitsExitWhenUnset(t *testing.T) {
	err := formatRunFailure(errors.New("boom"), &RunResult{Tasks: []TaskResult{{
		Name: "extract", Status: "failed", Attempt: 1, LogText: "stderr only\n",
	}}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "step extract attempt 1\n")
	require.NotContains(t, err.Error(), "exit ")
	require.Contains(t, err.Error(), "stderr only")
}

func TestFormatRunFailurePrintsScrubbedSnapshotOnly(t *testing.T) {
	const raw = "secret-token-SHOULD-NOT-APPEAR"
	code := 7
	err := formatRunFailure(errors.New("boom"), &RunResult{Tasks: []TaskResult{{
		Name:        "extract",
		Status:      "failed",
		Attempt:     1,
		ExitCode:    &code,
		LogScrubbed: true,
		LogText:     "token [REDACTED]",
	}}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "log (scrubbed):")
	require.Contains(t, err.Error(), "token [REDACTED]")
	require.NotContains(t, err.Error(), raw)
}

func TestFailureLogTailDropsHeadAndCapsBytes(t *testing.T) {
	lines := make([]string, failureLogTailLines+25)
	for i := range lines {
		lines[i] = fmt.Sprintf("line-%04d-MARKER", i)
	}
	tail := failureLogTail(strings.Join(lines, "\n"), false)
	require.NotContains(t, tail, "line-0000-MARKER")
	require.Contains(t, tail, fmt.Sprintf("line-%04d-MARKER", len(lines)-1))
	require.Contains(t, tail, "showing log tail")

	body := strings.Repeat("A", failureLogTailBytes*3) + "ENDMARKER"
	capped := failureLogTail(body, false)
	require.Contains(t, capped, "ENDMARKER")
	require.NotContains(t, capped, strings.Repeat("A", failureLogTailBytes))
	require.LessOrEqual(t, len(capped), failureLogTailBytes+len("[... showing log tail ...]\n"))
}

func TestFailureLogTailNotesSnapshotTruncation(t *testing.T) {
	tail := failureLogTail("kept head of a truncated snapshot\n", true)
	require.Contains(t, tail, "snapshot cap")
	require.Contains(t, tail, "kept head of a truncated snapshot")
}
