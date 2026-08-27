package run

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// replay_fanout_test.go pins the CLI's rendering of the server's fail-closed
// refusal of a fanned baseline (internal/replay.ErrFannedBaseline → 409).
// The refusal is correct but opaque on its own: an operator who reads only
// "replay refused (409)" learns nothing about what to do instead.

func TestReplay_FannedBaselineRefusalIsActionable(t *testing.T) {
	restoreReplayTestGlobals(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"quarantined replay refuses baselines containing fanned groups"}`))
	}))
	defer server.Close()

	replayJobID = "job-1"
	replayServer = server.URL
	replayAPIKey = ""
	replayJSON = false
	replayDiff = false
	replaySets = nil
	replayIdempotencyKey = "operator-key"

	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	cmd.Flags().String("idempotency-key", "", "")
	require.NoError(t, cmd.Flags().Set("idempotency-key", "operator-key"))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := runReplay(cmd, []string{"baseline-run"})
	require.Error(t, err)

	msg := err.Error()
	require.Contains(t, msg, "replay refused (409)")
	require.Contains(t, msg, "fanned groups", "the server's own words must survive")
	require.Contains(t, msg, "fail-closed", "say WHY it is refused, not just that it was")
	require.Contains(t, msg, "caesium run partitions", "name the command that inspects the group")
	require.Empty(t, stdout.String(), "a refusal writes nothing to stdout")
}

// TestReplay_OtherConflictsAreUnchanged keeps the added hint scoped to the
// fan-out refusal; every other 409 renders exactly as before.
func TestReplay_OtherConflictsAreUnchanged(t *testing.T) {
	err := replayStatusError(http.StatusConflict, []byte(`{"message":"baseline run is quarantined"}`))
	require.EqualError(t, err, "replay refused (409): baseline run is quarantined")
}
