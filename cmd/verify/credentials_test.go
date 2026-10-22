package verify

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/caesium-cloud/caesium/cmd/cliutil/credtest"
	"github.com/stretchr/testify/require"
)

// TestVerifySendsTheResolvedCredential proves `caesium verify` authenticates
// through cliutil.ResolveAPIKey (stored login, then CAESIUM_API_KEY, then
// --api-key).
func TestVerifySendsTheResolvedCredential(t *testing.T) {
	rec := credtest.Setup(t, http.StatusOK, `{}`)
	t.Cleanup(func() { verifyAPIKey = "" })
	path := filepath.Join(t.TempDir(), "receipt.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"job_id":"11111111-1111-1111-1111-111111111111","run_id":"22222222-2222-2222-2222-222222222222"}`), 0o600))
	run := func(extra ...string) {
		t.Helper()
		var out bytes.Buffer
		Cmd.SetOut(&out)
		Cmd.SetErr(&out)
		Cmd.SetArgs(append([]string{path, "--server", credtest.Server}, extra...))
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
