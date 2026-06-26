package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

func TestRunReplayDiffRendersDiffThenErrorsForFailedReplay(t *testing.T) {
	restoreReplayTestGlobals(t)

	const (
		jobID    = "job-1"
		baseline = "baseline-run"
		replayID = "replay-run"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/jobs/"+jobID+"/runs/"+baseline+"/replay":
			if got := r.Header.Get("Idempotency-Key"); got != "operator-key" {
				http.Error(w, "unexpected idempotency key: "+got, http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"run_id":"` + replayID + `","status":"running","quarantine":true}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/"+jobID+"/runs/"+replayID:
			_, _ = w.Write([]byte(`{"id":"` + replayID + `","status":"failed"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/"+jobID+"/runs/diff":
			if got := r.URL.Query().Get("left"); got != baseline {
				http.Error(w, "unexpected left run: "+got, http.StatusBadRequest)
				return
			}
			if got := r.URL.Query().Get("right"); got != replayID {
				http.Error(w, "unexpected right run: "+got, http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{
				"jobId":"job-1",
				"leftRunId":"baseline-run",
				"rightRunId":"replay-run",
				"leftStatus":"succeeded",
				"rightStatus":"failed",
				"tasks":[{"taskName":"deploy","leftStatus":"succeeded","rightStatus":"failed","verdict":"changed","hashEqual":false}]
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	replayJobID = jobID
	replayServer = server.URL
	replayAPIKey = ""
	replayJSON = true
	replayDiff = true
	replaySets = nil
	replayIdempotencyKey = "operator-key"

	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	cmd.Flags().String("idempotency-key", "", "")
	require.NoError(t, cmd.Flags().Set("idempotency-key", "operator-key"))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := runReplay(cmd, []string{baseline})
	require.Error(t, err)
	require.EqualError(t, err, "replay run "+replayID+" failed")
	require.Contains(t, stderr.String(), "awaiting replay run "+replayID)
	require.True(t, json.Valid(stdout.Bytes()), "stdout was not JSON:\n%s", stdout.String())
	require.Contains(t, stdout.String(), `"rightRunId": "replay-run"`)
	require.Contains(t, stdout.String(), `"rightStatus": "failed"`)
}

func restoreReplayTestGlobals(t *testing.T) {
	t.Helper()
	originalJobID := replayJobID
	originalServer := replayServer
	originalAPIKey := replayAPIKey
	originalJSON := replayJSON
	originalDiff := replayDiff
	originalSets := replaySets
	originalIDKey := replayIdempotencyKey
	t.Cleanup(func() {
		replayJobID = originalJobID
		replayServer = originalServer
		replayAPIKey = originalAPIKey
		replayJSON = originalJSON
		replayDiff = originalDiff
		replaySets = originalSets
		replayIdempotencyKey = originalIDKey
	})
}
