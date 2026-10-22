package receipt

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/caesium-cloud/caesium/cmd/cliutil/credtest"
	"github.com/stretchr/testify/require"
)

// TestGetSendsTheResolvedCredential proves `caesium receipt get`
// authenticates through cliutil.ResolveAPIKey (stored login, then
// CAESIUM_API_KEY, then --api-key).
func TestGetSendsTheResolvedCredential(t *testing.T) {
	rec := credtest.Setup(t, http.StatusOK, `{}`)
	t.Cleanup(func() { getAPIKey = "" })
	run := func(extra ...string) {
		t.Helper()
		var out bytes.Buffer
		Cmd.SetOut(&out)
		Cmd.SetErr(&out)
		Cmd.SetArgs(append([]string{"get", "--job-id", "j1", "--run-id", "r1", "--server", credtest.Server}, extra...))
		_ = Cmd.ExecuteContext(context.Background())
	}
	run()
	require.Equal(t, "Bearer "+credtest.StoredKey, rec.Last(t))
	t.Setenv("CAESIUM_API_KEY", "env-key")
	run()
	require.Equal(t, "Bearer env-key", rec.Last(t))
	run("--api-key", "flag-key")
	require.Equal(t, "Bearer flag-key", rec.Last(t))
}
