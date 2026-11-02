package run

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestRunGetPrintsStatusAliasAndTasks(t *testing.T) {
	restoreInspectGlobals(t)
	const body = `{"id":"run-1","job_id":"job-1","job_alias":"nightly","status":"succeeded","tasks":[{"id":"task-1","task_id":"task-1","status":"succeeded"}]}`
	wantAuth := "Bearer secret-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/v1/jobs/job-1/runs/run-1", r.URL.Path)
		require.Equal(t, wantAuth, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	getJobID, getRunID, getServer, getAPIKey = "job-1", "run-1", server.URL, "secret-key"

	stdout, stderr, err := execInspect(t, runGet)
	require.NoError(t, err)
	require.Contains(t, stdout, "status: succeeded")
	require.Contains(t, stdout, "nightly")
	require.Contains(t, stdout, "job-1")
	require.Contains(t, stdout, "task-1 succeeded")
	require.Contains(t, stdout, "tasks: total=1 succeeded=1 failed=0")
	require.NotContains(t, stdout, "warning:")
	require.Contains(t, stderr, "warning: --api-key")

	getAsJSON = true
	getAPIKey = ""
	wantAuth = ""
	stdout, stderr, err = execInspect(t, runGet)
	require.NoError(t, err)
	require.Empty(t, stderr)
	doc := decodeStatus(t, stdout)
	require.Equal(t, "succeeded", doc.Status)
	require.Equal(t, "nightly", doc.JobAlias)
	require.Equal(t, "job-1", doc.JobID)
	require.Equal(t, 1, doc.TaskSummary.Succeeded)
	require.Equal(t, 0, doc.TaskSummary.Failed)
	require.Len(t, doc.Tasks, 1)
}

func TestRunWaitSuccessFailureAndTimeout(t *testing.T) {
	restoreInspectGlobals(t)
	waitPollInterval = time.Millisecond

	t.Run("success", func(t *testing.T) {
		calls := 0
		server := inspectServer(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			status := "running"
			if calls > 1 {
				status = "succeeded"
			}
			writeRun(w, status, `[{"id":"task-1","status":"succeeded"}]`)
		})
		waitJobID, waitRunID, waitServer, waitTimeout, waitJSON = "job-1", "run-1", server.URL, 2*time.Second, false
		stdout, stderr, err := execInspect(t, runWait)
		require.NoError(t, err)
		require.Equal(t, "succeeded\n", stdout)
		require.Contains(t, stderr, "nightly")
		require.GreaterOrEqual(t, calls, 2)
	})

	t.Run("run failure", func(t *testing.T) {
		for _, status := range []string{"failed", "cancelled", "skipped"} {
			server := inspectServer(t, func(w http.ResponseWriter, r *http.Request) {
				writeRun(w, status, `[{"id":"task-1","status":"`+status+`"}]`)
			})
			waitJobID, waitRunID, waitServer, waitTimeout, waitJSON = "job-1", "run-1", server.URL, 2*time.Second, false
			stdout, _, err := execInspect(t, runWait)
			require.Error(t, err)
			require.ErrorContains(t, err, status)
			require.Equal(t, status+"\n", stdout)
		}
	})

	t.Run("task failure", func(t *testing.T) {
		server := inspectServer(t, func(w http.ResponseWriter, r *http.Request) {
			writeRun(w, "succeeded", `[{"id":"task-1","status":"failed","partition_count":2,"partition_status_counts":{"succeeded":1,"failed":1}}]`)
		})
		waitJobID, waitRunID, waitServer, waitTimeout, waitJSON = "job-1", "run-1", server.URL, 2*time.Second, true
		stdout, stderr, err := execInspect(t, runWait)
		require.Error(t, err)
		require.ErrorContains(t, err, "task(s) failed")
		require.NotContains(t, stderr, "{")
		doc := decodeStatus(t, stdout)
		require.Equal(t, "succeeded", doc.Status)
		require.False(t, doc.TimedOut)
		require.Equal(t, 1, doc.TaskSummary.Failed)
		require.Equal(t, 1, doc.TaskSummary.Succeeded)
		require.Equal(t, 2, doc.TaskSummary.Total)
	})

	t.Run("timeout", func(t *testing.T) {
		waitPollInterval = time.Hour
		server := inspectServer(t, func(w http.ResponseWriter, r *http.Request) {
			writeRun(w, "running", `[{"id":"task-1","status":"running"}]`)
		})
		waitJobID, waitRunID, waitServer, waitTimeout, waitJSON = "job-1", "run-1", server.URL, 40*time.Millisecond, true
		stdout, stderr, err := execInspect(t, runWait)
		require.Error(t, err)
		require.ErrorContains(t, err, "timed out")
		require.NotContains(t, stderr, "running")
		doc := decodeStatus(t, stdout)
		require.Equal(t, "running", doc.Status)
		require.True(t, doc.TimedOut)
		require.NotContains(t, stdout, "timed out")
	})
}

func TestRunLogsPrintsTasksPartitionsAndBounds(t *testing.T) {
	restoreInspectGlobals(t)

	t.Run("plain log", func(t *testing.T) {
		var queries []string
		server := inspectServer(t, func(w http.ResponseWriter, r *http.Request) {
			queries = append(queries, r.URL.RawQuery)
			if strings.Contains(r.URL.Path, "/logs") {
				require.Equal(t, "task-1", r.URL.Query().Get("task_id"))
				require.Empty(t, r.URL.Query().Get("task_run_id"))
				w.Header().Set("X-Caesium-Log-Source", "persisted")
				_, _ = w.Write([]byte("hello [REDACTED]\n"))
				return
			}
			writeRun(w, "succeeded", `[{"id":"task-1","status":"succeeded"}]`)
		})
		logsJobID, logsRunID, logsServer, logsJSON = "job-1", "run-1", server.URL, false
		stdout, stderr, err := execInspect(t, runLogs)
		require.NoError(t, err)
		require.Contains(t, stdout, "== task-1 ==")
		require.Contains(t, stdout, "hello [REDACTED]")
		require.NotContains(t, stderr, "truncated")
		for _, query := range queries {
			require.NotContains(t, query, "raw")
			require.NotContains(t, query, "unredacted")
		}
	})

	t.Run("partitions", func(t *testing.T) {
		logsMaxInstances = 1
		var selected []string
		server := inspectServer(t, func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.URL.Path, "/logs") {
				writeRun(w, "succeeded", `[{"id":"task-fan","status":"failed","partition_count":3}]`)
				return
			}
			require.Equal(t, "task-fan", r.URL.Query().Get("task_id"))
			for key := range r.URL.Query() {
				require.Contains(t, []string{"task_id", "task_run_id"}, key)
			}
			if r.URL.Query().Get("task_run_id") == "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"message":"select one","partition_count":3,"instances":[{"task_run_id":"tr-a","partition":"a","status":"succeeded"},{"task_run_id":"tr-b","partition":"b","status":"failed"},{"task_run_id":"tr-c","partition":"c","status":"succeeded"}]}`))
				return
			}
			selected = append(selected, r.URL.Query().Get("task_run_id"))
			w.Header().Set("X-Caesium-Partition", "a")
			w.Header().Set("X-Caesium-Task-Run-ID", r.URL.Query().Get("task_run_id"))
			_, _ = w.Write([]byte("partition=a secret=[REDACTED]\n"))
		})
		logsJobID, logsRunID, logsServer, logsJSON = "job-1", "run-1", server.URL, true
		stdout, stderr, err := execInspect(t, runLogs)
		require.NoError(t, err)
		require.Equal(t, []string{"tr-a"}, selected, "the instance cap must stop the walk")
		require.Contains(t, stderr, "showing 1 of 3")
		require.Contains(t, stderr, "truncated")
		require.NotContains(t, stdout, "showing 1 of 3")
		var doc logsDocument
		require.NoError(t, json.Unmarshal([]byte(stdout), &doc))
		require.True(t, doc.Truncated)
		require.Len(t, doc.Entries, 1)
		require.Equal(t, "a", doc.Entries[0].Partition)
		require.Equal(t, "tr-a", doc.Entries[0].TaskRunID)
		require.Contains(t, doc.Entries[0].Text, "[REDACTED]")
		require.NotContains(t, doc.Entries[0].Text, "partition=b")
	})

	t.Run("byte cap", func(t *testing.T) {
		logsMaxBytes = 4
		server := inspectServer(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/logs") {
				_, _ = w.Write([]byte("abcdefghij"))
				return
			}
			writeRun(w, "succeeded", `[{"id":"task-1","status":"succeeded"}]`)
		})
		logsJobID, logsRunID, logsServer, logsJSON = "job-1", "run-1", server.URL, false
		stdout, stderr, err := execInspect(t, runLogs)
		require.NoError(t, err)
		require.Contains(t, stdout, "abcd")
		require.NotContains(t, stdout, "efgh")
		require.Contains(t, stderr, "truncated")
	})
}

func restoreInspectGlobals(t *testing.T) {
	t.Helper()
	origGetRun, origGetJob, origGetServer, origGetKey, origGetJSON := getRunID, getJobID, getServer, getAPIKey, getAsJSON
	origWaitRun, origWaitJob, origWaitServer, origWaitKey, origWaitJSON := waitRunID, waitJobID, waitServer, waitAPIKey, waitJSON
	origWaitTimeout, origPoll := waitTimeout, waitPollInterval
	origLogsRun, origLogsJob, origLogsServer, origLogsKey, origLogsJSON := logsRunID, logsJobID, logsServer, logsAPIKey, logsJSON
	origBytes, origInstances, origFetch := logsMaxBytes, logsMaxInstances, logsFetchTimeout
	t.Cleanup(func() {
		getRunID, getJobID, getServer, getAPIKey, getAsJSON = origGetRun, origGetJob, origGetServer, origGetKey, origGetJSON
		waitRunID, waitJobID, waitServer, waitAPIKey, waitJSON = origWaitRun, origWaitJob, origWaitServer, origWaitKey, origWaitJSON
		waitTimeout, waitPollInterval = origWaitTimeout, origPoll
		logsRunID, logsJobID, logsServer, logsAPIKey, logsJSON = origLogsRun, origLogsJob, origLogsServer, origLogsKey, origLogsJSON
		logsMaxBytes, logsMaxInstances, logsFetchTimeout = origBytes, origInstances, origFetch
	})
	getAPIKey, waitAPIKey, logsAPIKey = "", "", ""
	getAsJSON, waitJSON, logsJSON = false, false, false
	t.Setenv(runDiffAPIKeyEnvVar, "")
}

func execInspect(t *testing.T, run func(*cobra.Command, []string) error) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(ctx)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	err := run(cmd, nil)
	return stdout.String(), stderr.String(), err
}

func inspectServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func writeRun(w http.ResponseWriter, status, tasks string) {
	_, _ = w.Write([]byte(`{"id":"run-1","job_id":"job-1","job_alias":"nightly","status":"` + status + `","tasks":` + tasks + `}`))
}

func decodeStatus(t *testing.T, stdout string) runStatusDocument {
	t.Helper()
	require.True(t, json.Valid([]byte(stdout)), "stdout was not JSON:\n%s", stdout)
	var doc runStatusDocument
	require.NoError(t, json.Unmarshal([]byte(stdout), &doc))
	return doc
}
