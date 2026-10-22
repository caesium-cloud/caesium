package backfill

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/caesium-cloud/caesium/cmd/cliutil/credtest"
	"github.com/stretchr/testify/require"
)

// TestSubcommandsSendTheResolvedCredential proves every backfill subcommand
// authenticates through cliutil.ResolveAPIKey: the stored login for its
// --server, CAESIUM_API_KEY over it, and the inherited --api-key over both.
func TestSubcommandsSendTheResolvedCredential(t *testing.T) {
	rec := credtest.Setup(t, http.StatusOK, `{}`)
	t.Cleanup(func() { apiKeyFlag = "" })
	run := func(args ...string) {
		t.Helper()
		var out bytes.Buffer
		Cmd.SetOut(&out)
		Cmd.SetErr(&out)
		Cmd.SetArgs(append(args, "--server", credtest.Server))
		_ = Cmd.ExecuteContext(context.Background())
	}
	leaves := [][]string{
		{"list", "--job-id", "j1"},
		{"create", "--job-id", "j1", "--start", "2026-01-01T00:00:00Z", "--end", "2026-01-02T00:00:00Z"},
		{"cancel", "--job-id", "j1", "--backfill-id", "b1"},
	}
	for _, leaf := range leaves {
		run(leaf...)
		require.Equal(t, "Bearer "+credtest.StoredKey, rec.Last(t), "%v uses the stored login", leaf)
	}
	t.Setenv("CAESIUM_API_KEY", "env-key")
	for _, leaf := range leaves {
		run(leaf...)
		require.Equal(t, "Bearer env-key", rec.Last(t), "%v: the env var beats the stored login", leaf)
	}
	for _, leaf := range leaves {
		run(append(leaf, "--api-key", "flag-key")...)
		require.Equal(t, "Bearer flag-key", rec.Last(t), "%v: the inherited flag beats the env var", leaf)
	}
}
