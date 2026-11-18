package run

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/stretchr/testify/require"
)

func TestRunCancelJSONStdoutIsClean(t *testing.T) {
	restoreCancelGlobals(t)
	var sawPath, sawMethod, sawAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath, sawMethod, sawAuth = r.URL.Path, r.Method, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"run-1","job_id":"job-1","status":"cancelled","error":"cancelled by operator","tasks":[{"id":"task-ok","task_id":"task-ok","status":"succeeded"},{"id":"task-hold","task_id":"task-hold","status":"cancelled"}]}`))
	}))
	t.Cleanup(server.Close)

	cancelJobID, cancelRunID, cancelServer, cancelAPIKey, cancelAsJSON = "job-1", "run-1", server.URL, "secret", true
	stdout, stderr, err := execInspect(t, runCancel)
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, sawMethod)
	require.Equal(t, "/v1/jobs/job-1/runs/run-1/cancel", sawPath)
	require.Equal(t, "Bearer secret", sawAuth)
	require.True(t, json.Valid([]byte(stdout)), "stdout was not JSON:\n%s", stdout)
	require.NotContains(t, stdout, "Cancellation is not a task failure")
	require.NotContains(t, stdout, "warning:")
	require.Contains(t, stderr, "warning: --api-key is visible in process listings")
	require.Contains(t, stderr, "Cancellation is not a task failure")
	require.Contains(t, stderr, "CAESIUM_TASK_FAILURE_POLICY is unchanged")

	var doc runView
	require.NoError(t, json.Unmarshal([]byte(stdout), &doc))
	require.Equal(t, "cancelled", doc.Status)
	require.Equal(t, "run-1", doc.ID)
	require.Equal(t, "succeeded", doc.Tasks[0].Status)
	require.Equal(t, "cancelled", doc.Tasks[1].Status)
}

func TestRunCancelHumanAndConflict(t *testing.T) {
	restoreCancelGlobals(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "missing") {
			http.Error(w, `{"error":"not_found","message":"run not found"}`, http.StatusNotFound)
			return
		}
		if strings.Contains(r.URL.Path, "done") {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"run_not_running","message":"only a running run can be cancelled","status":"succeeded"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"run-1","job_id":"job-1","status":"cancelled"}`))
	}))
	t.Cleanup(server.Close)

	cancelJobID, cancelRunID, cancelServer, cancelAsJSON = "job-1", "run-1", server.URL, false
	stdout, stderr, err := execInspect(t, runCancel)
	require.NoError(t, err)
	require.Equal(t, "Cancelled run run-1 (job job-1)\n", stdout)
	require.NotContains(t, stdout, "{")
	require.Contains(t, stderr, "tolerant trigger rule")
	require.NotContains(t, stderr, "warning:")

	cancelRunID = "done"
	stdout, stderr, err = execInspect(t, runCancel)
	require.Error(t, err)
	require.Empty(t, stdout)
	require.NotContains(t, stderr, "Cancellation is not a task failure")
	require.Contains(t, err.Error(), "only a running run can be cancelled")

	cancelRunID = "missing"
	stdout, _, err = execInspect(t, runCancel)
	require.Error(t, err)
	require.Empty(t, stdout)
	require.Contains(t, err.Error(), "run cancel failed (404)")

	cancelJobID = ""
	_, _, err = execInspect(t, runCancel)
	require.EqualError(t, err, "--job-id is required")
}

func restoreCancelGlobals(t *testing.T) {
	t.Helper()
	origRun, origJob, origServer, origKey, origJSON := cancelRunID, cancelJobID, cancelServer, cancelAPIKey, cancelAsJSON
	t.Cleanup(func() {
		cancelRunID, cancelJobID, cancelServer, cancelAPIKey, cancelAsJSON = origRun, origJob, origServer, origKey, origJSON
	})
	cancelAPIKey = ""
	cancelAsJSON = false
	t.Setenv(cliutil.APIKeyEnvVar, "")
}
