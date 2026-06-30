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
	originalClient := startHTTPClient
	t.Cleanup(func() {
		startJobID = originalJobID
		startServer = originalServer
		startAPIKey = originalAPIKey
		startParams = originalParams
		startPriority = originalPriority
		startHTTPClient = originalClient
	})
}
