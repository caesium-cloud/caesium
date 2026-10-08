package auth

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestLeafCommandsRejectUnexpectedArgumentBeforeRun(t *testing.T) {
	for _, leaf := range []*cobra.Command{auditCmd, keyCreateCmd, keyListCmd, keyRevokeCmd, keyRotateCmd} {
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

func TestAPIKeyResolverPolicy(t *testing.T) {
	for _, tc := range []struct{ name, flag, env, want, warning string }{
		{"trimmed flag precedence", " flag ", " env ", "flag", "warning: --api-key is visible in process listings; prefer CAESIUM_API_KEY\n"},
		{"trimmed environment", "  ", " env ", "env", ""},
		{"empty", "", "  ", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CAESIUM_API_KEY", tc.env)
			var stdout, stderr bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			require.Equal(t, tc.want, resolveAPIKey(cmd, tc.flag))
			require.Empty(t, stdout.String())
			require.Equal(t, tc.warning, stderr.String())
		})
	}
}
