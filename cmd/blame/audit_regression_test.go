package blame

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
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

func TestBlameOutputUsesConfiguredWriter(t *testing.T) {
	oldServer, oldKey, oldJSON := blameServer, blameAPIKey, blameJSON
	oldTask, oldFrom, oldTo := blameTask, blameFrom, blameTo
	t.Cleanup(func() {
		blameServer, blameAPIKey, blameJSON = oldServer, oldKey, oldJSON
		blameTask, blameFrom, blameTo = oldTask, oldFrom, oldTo
	})
	blameAPIKey, blameTask, blameFrom, blameTo = "", "", "", ""
	t.Setenv(apiKeyEnvVar, "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/jobs/11111111-1111-1111-1111-111111111111/blame", r.URL.Path)
		_, _ = w.Write([]byte(`{"coverage":"complete","tasks":[],"edges":[]}`))
	}))
	defer server.Close()
	blameServer = server.URL
	for _, jsonOutput := range []bool{true, false} {
		blameJSON = jsonOutput
		var stdout, stderr bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		require.NoError(t, Cmd.RunE(cmd, []string{"11111111-1111-1111-1111-111111111111"}))
		require.Empty(t, stderr.String())
		if jsonOutput {
			require.Equal(t, "{\n  \"coverage\": \"complete\",\n  \"edges\": [],\n  \"tasks\": []\n}\n", stdout.String())
		} else {
			require.Contains(t, stdout.String(), coverageNote)
			require.Contains(t, stdout.String(), "coverage: complete")
			require.Contains(t, stdout.String(), "INTRODUCING COMMIT")
		}
	}
}
