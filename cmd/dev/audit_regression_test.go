package dev

import (
	"bytes"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestLeafCommandsRejectUnexpectedArgumentBeforeRun(t *testing.T) {
	for _, leaf := range []*cobra.Command{Cmd} {
		t.Run(leaf.Name(), func(t *testing.T) {
			require.NotNil(t, leaf.Args)
			ran := false
			probe := &cobra.Command{Use: leaf.Use, Args: leaf.Args, SilenceErrors: true, SilenceUsage: true, RunE: func(*cobra.Command, []string) error { ran = true; return nil }}
			var stdout, stderr bytes.Buffer
			probe.SetOut(&stdout)
			probe.SetErr(&stderr)
			probe.SetArgs([]string{"unexpected"})
			require.Error(t, probe.Execute())
			require.False(t, ran)
			require.Empty(t, stdout.String())
		})
	}
}
