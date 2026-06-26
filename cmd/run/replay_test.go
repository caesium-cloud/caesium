package run

import (
	"bytes"
	"errors"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestParseReplaySet(t *testing.T) {
	got, err := parseReplaySet([]string{"mode=what-if", "flavor=vanilla=bean", " spaced =value"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"mode":   "what-if",
		"flavor": "vanilla=bean",
		"spaced": "value",
	}, got)
}

func TestParseReplaySetRejectsMalformedValues(t *testing.T) {
	for _, input := range [][]string{
		{"missing-equals"},
		{"=value"},
		{"   =value"},
	} {
		_, err := parseReplaySet(input)
		require.Error(t, err)
		require.Contains(t, err.Error(), "--set must be k=v")
	}
}

func TestResolveReplayIdempotencyKeyPrintsGeneratedKeyToStderr(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)

	key, err := resolveReplayIdempotencyKey(cmd, "", false, func() (string, error) {
		return "generated-key", nil
	})
	require.NoError(t, err)
	require.Equal(t, "generated-key", key)
	require.Equal(t, "replay idempotency key: generated-key\n", stderr.String())
}

func TestResolveReplayIdempotencyKeyPassesOperatorValueVerbatim(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)

	key, err := resolveReplayIdempotencyKey(cmd, "  operator key  ", true, func() (string, error) {
		return "", errors.New("must not generate")
	})
	require.NoError(t, err)
	require.Equal(t, "  operator key  ", key)
	require.Empty(t, stderr.String())
}
