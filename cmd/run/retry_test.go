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

func TestRetrySinglePartitionSendsAuthHeaderOnBothRequests(t *testing.T) {
	restoreRetryTestGlobals(t)

	const (
		jobID = "job-1"
		runID = "run-1"
		task  = "process-file"
	)
	var sawListAuth, sawRetryAuth bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/"+jobID+"/runs/"+runID+"/tasks/"+task+"/partitions":
			sawListAuth = r.Header.Get("Authorization") == "Bearer secret-key"
			_, _ = w.Write([]byte(`{"partitions":[{"value":"bravo","index":1,"status":"failed"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/jobs/"+jobID+"/runs/"+runID+"/tasks/"+task+"/partitions/1/retry":
			sawRetryAuth = r.Header.Get("Authorization") == "Bearer secret-key"
			_, _ = w.Write([]byte(`{"retried":true,"index":1,"value":"bravo"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	retryFromFailureServer = server.URL
	retryFromFailureAPIKey = "secret-key"

	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := retrySinglePartition(context.Background(), cmd, jobID, runID, task, "bravo")
	require.NoError(t, err)
	require.True(t, sawListAuth, "expected Authorization header on the partitions list request")
	require.True(t, sawRetryAuth, "expected Authorization header on the retry request")
	require.Contains(t, stdout.String(), "Retried partition")
	require.Contains(t, stderr.String(), "warning: --api-key is visible in process listings")
}

func TestRetrySinglePartitionNotFound(t *testing.T) {
	restoreRetryTestGlobals(t)

	const (
		jobID = "job-1"
		runID = "run-1"
		task  = "process-file"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"partitions":[{"value":"alpha","index":0,"status":"succeeded"}]}`))
	}))
	defer server.Close()

	retryFromFailureServer = server.URL
	retryFromFailureAPIKey = ""

	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := retrySinglePartition(context.Background(), cmd, jobID, runID, task, "missing")
	require.Error(t, err)
	require.Contains(t, err.Error(), `partition "missing" not found`)
}

func restoreRetryTestGlobals(t *testing.T) {
	t.Helper()
	originalJobID := retryFromFailureJobID
	originalRunID := retryFromFailureRunID
	originalPartition := retryFromFailurePartition
	originalTask := retryFromFailureTask
	originalServer := retryFromFailureServer
	originalAPIKey := retryFromFailureAPIKey
	t.Cleanup(func() {
		retryFromFailureJobID = originalJobID
		retryFromFailureRunID = originalRunID
		retryFromFailurePartition = originalPartition
		retryFromFailureTask = originalTask
		retryFromFailureServer = originalServer
		retryFromFailureAPIKey = originalAPIKey
	})
}
