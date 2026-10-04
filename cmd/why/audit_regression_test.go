package why

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

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
