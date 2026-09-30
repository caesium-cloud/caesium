package run

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestRunStartPostsPriorityAndPrintsRunID(t *testing.T) {
	restoreStartTestGlobals(t)

	const (
		jobID = "job-1"
		runID = "run-1"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/jobs/"+jobID+"/run", r.URL.Path)
		require.Equal(t, "Bearer secret-key", r.Header.Get("Authorization"))

		var req startRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		require.Equal(t, map[string]string{"branch": "main"}, req.Params)
		require.Equal(t, "high", req.Priority)

		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"` + runID + `","priority":3}`))
	}))
	defer server.Close()

	startJobID = jobID
	startServer = server.URL
	startAPIKey = "secret-key"
	startParams = []string{"branch=main"}
	startPriority = "high"

	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	require.NoError(t, runStart(cmd, nil))
	require.Equal(t, runID+"\n", stdout.String())
	require.Contains(t, stderr.String(), "warning: --api-key is visible in process listings")
}

func TestParseRunStartParamsRejectsMalformedValues(t *testing.T) {
	for _, input := range [][]string{
		{"missing-equals"},
		{"=value"},
		{"   =value"},
	} {
		_, err := parseRunStartParams(input)
		require.Error(t, err)
		require.Contains(t, err.Error(), "--params must be k=v")
	}
}

func restoreStartTestGlobals(t *testing.T) {
	t.Helper()
	originalJobID := startJobID
	originalServer := startServer
	originalAPIKey := startAPIKey
	originalParams := startParams
	originalPriority := startPriority
	originalIdempotencyKey := startIdempotencyKey
	originalClient := startHTTPClient
	t.Cleanup(func() {
		startJobID = originalJobID
		startServer = originalServer
		startAPIKey = originalAPIKey
		startParams = originalParams
		startPriority = originalPriority
		startIdempotencyKey = originalIdempotencyKey
		startHTTPClient = originalClient
	})
}

// runStartAgainst points the start command at a stub server answering with
// status, headers and body, and returns the command's stdout, stderr and error.
func runStartAgainst(t *testing.T, key string, status int, headers map[string]string, body string) (string, string, error) {
	t.Helper()
	restoreStartTestGlobals(t)

	var gotKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("Idempotency-Key")
		for name, value := range headers {
			w.Header().Set(name, value)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	startJobID = "job-1"
	startServer = server.URL
	startAPIKey = ""
	startParams = nil
	startPriority = ""
	startIdempotencyKey = key

	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	err := runStart(cmd, nil)
	require.Equal(t, key, gotKey, "the key must travel as the Idempotency-Key header")
	return stdout.String(), stderr.String(), err
}

func TestRunStartSendsIdempotencyKeyAndKeepsStdoutToRunID(t *testing.T) {
	stdout, stderr, err := runStartAgainst(t, "wf-1/act-1", http.StatusAccepted,
		map[string]string{"Idempotent-Replayed": "true"},
		`{"id":"run-1","outcome":"created","status":"succeeded"}`)
	require.NoError(t, err)
	require.Equal(t, "run-1\n", stdout, "stdout carries the run ID and nothing else")
	require.Contains(t, stderr, `idempotency key "wf-1/act-1" matched an earlier start`)
}

func TestRunStartWithoutKeySendsNoHeader(t *testing.T) {
	stdout, _, err := runStartAgainst(t, "", http.StatusAccepted, nil, `{"id":"run-1","outcome":"created"}`)
	require.NoError(t, err)
	require.Equal(t, "run-1\n", stdout)
}

func TestRunStartQueuedPrintsNothingOnStdout(t *testing.T) {
	stdout, stderr, err := runStartAgainst(t, "k", http.StatusAccepted, nil,
		`{"outcome":"queued","job_id":"job-1","queue_id":"q-1"}`)
	require.NoError(t, err, "a queued start will run; it is not a failure")
	require.Empty(t, stdout)
	require.Contains(t, stderr, "queue id q-1")
	require.Contains(t, stderr, "rerun with the same --idempotency-key")
}

func TestRunStartSkippedAndDroppedFail(t *testing.T) {
	_, _, err := runStartAgainst(t, "", http.StatusAccepted, nil,
		`{"outcome":"skipped","job_id":"job-1","reason":"max_concurrency"}`)
	require.ErrorContains(t, err, "skipped (max_concurrency)")

	_, _, err = runStartAgainst(t, "", http.StatusAccepted, nil,
		`{"outcome":"skipped","job_id":"job-1","reason":"dataset_hold","run_id":"run-9"}`)
	require.ErrorContains(t, err, "skipped run run-9")

	_, _, err = runStartAgainst(t, "k", http.StatusAccepted, nil,
		`{"outcome":"dropped","job_id":"job-1","queue_id":"q-1"}`)
	require.ErrorContains(t, err, "queue entry was removed")
}

func TestRunStartToleratesEmptyAcceptedBody(t *testing.T) {
	stdout, stderr, err := runStartAgainst(t, "", http.StatusAccepted, nil, "")
	require.NoError(t, err)
	require.Empty(t, stdout)
	require.Contains(t, stderr, "without creating a run")
}

func TestRunStartReportsReusedKey(t *testing.T) {
	_, _, err := runStartAgainst(t, "k", http.StatusUnprocessableEntity, nil,
		`{"message":"idempotency key was already used for a different request"}`)
	require.ErrorContains(t, err, "422")
	require.ErrorContains(t, err, "already used for a different request")
}
