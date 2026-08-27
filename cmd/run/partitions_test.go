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

func TestRunPartitionsSendsAuthHeaderAndJSONOutputIsClean(t *testing.T) {
	restorePartitionsTestGlobals(t)

	const (
		jobID = "job-1"
		runID = "run-1"
		task  = "process-file"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/v1/jobs/"+jobID+"/runs/"+runID+"/tasks/"+task+"/partitions", r.URL.Path)
		require.Equal(t, "Bearer secret-key", r.Header.Get("Authorization"))

		_, _ = w.Write([]byte(`{"partitions":[
			{"value":"alpha","index":0,"status":"succeeded","attempt":1,"cache_hit":false,"fingerprint":"sha256:` + sixtyFourHex('a') + `","depends_on":[]},
			{"value":"bravo","index":1,"status":"failed","attempt":2,"cache_hit":false,"error":"boom","fingerprint":"sha256:` + sixtyFourHex('b') + `","depends_on":["alpha"]}
		]}`))
	}))
	defer server.Close()

	partitionsJobID = jobID
	partitionsTask = task
	partitionsServer = server.URL
	partitionsAPIKey = "secret-key"
	partitionsJSON = true

	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	require.NoError(t, runPartitions(cmd, []string{runID}))
	require.True(t, json.Valid(stdout.Bytes()), "stdout was not valid JSON:\n%s", stdout.String())
	require.Contains(t, stdout.String(), `"fingerprint"`)
	require.Contains(t, stdout.String(), `"depends_on"`)
	require.Contains(t, stderr.String(), "warning: --api-key is visible in process listings")
}

func TestRunPartitionsRendersHumanTable(t *testing.T) {
	restorePartitionsTestGlobals(t)

	const (
		jobID = "job-1"
		runID = "run-1"
		task  = "process-file"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"partitions":[
			{"value":"alpha","index":0,"status":"succeeded","attempt":1,"cache_hit":true,"duration":"12ms"},
			{"value":"bravo","index":1,"status":"failed","attempt":2,"cache_hit":false,"error":"boom"}
		]}`))
	}))
	defer server.Close()

	partitionsJobID = jobID
	partitionsTask = task
	partitionsServer = server.URL
	partitionsAPIKey = ""
	partitionsJSON = false

	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	require.NoError(t, runPartitions(cmd, []string{runID}))
	require.False(t, json.Valid(stdout.Bytes()), "table output should not be JSON:\n%s", stdout.String())

	out := stdout.String()
	require.Contains(t, out, "VALUE")
	require.Contains(t, out, "STATUS")
	require.Contains(t, out, "CACHE HIT")
	require.Contains(t, out, "alpha")
	require.Contains(t, out, "bravo")
	require.Contains(t, out, "boom")
	// Neither row carries a fingerprint or depends_on, so those columns must
	// not appear.
	require.NotContains(t, out, "FINGERPRINT")
	require.NotContains(t, out, "DEPENDS ON")
}

func TestRunPartitionsTableShowsFingerprintAndDependsOnColumnsWhenPresent(t *testing.T) {
	restorePartitionsTestGlobals(t)

	const (
		jobID = "job-1"
		runID = "run-1"
		task  = "process-file"
	)
	digest := "sha256:" + sixtyFourHex('c')
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"partitions":[
			{"value":"alpha","index":0,"status":"succeeded","attempt":1,"cache_hit":false,"fingerprint":"` + digest + `"},
			{"value":"bravo","index":1,"status":"succeeded","attempt":1,"cache_hit":false,"depends_on":["alpha"]}
		]}`))
	}))
	defer server.Close()

	partitionsJobID = jobID
	partitionsTask = task
	partitionsServer = server.URL
	partitionsAPIKey = ""
	partitionsJSON = false

	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	require.NoError(t, runPartitions(cmd, []string{runID}))

	out := stdout.String()
	require.Contains(t, out, "FINGERPRINT")
	require.Contains(t, out, "DEPENDS ON")
	require.Contains(t, out, digest)
	require.Contains(t, out, "alpha")
}

func TestRunPartitionsRequiresJobIDAndTask(t *testing.T) {
	restorePartitionsTestGlobals(t)

	partitionsJobID = ""
	partitionsTask = ""

	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())

	err := runPartitions(cmd, []string{"run-1"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "--job-id and --task are required")
}

func sixtyFourHex(b byte) string {
	out := make([]byte, 64)
	for i := range out {
		out[i] = b
	}
	return string(out)
}

func restorePartitionsTestGlobals(t *testing.T) {
	t.Helper()
	originalJobID := partitionsJobID
	originalTask := partitionsTask
	originalStatus := partitionsStatus
	originalJSON := partitionsJSON
	originalServer := partitionsServer
	originalAPIKey := partitionsAPIKey
	t.Cleanup(func() {
		partitionsJobID = originalJobID
		partitionsTask = originalTask
		partitionsStatus = originalStatus
		partitionsJSON = originalJSON
		partitionsServer = originalServer
		partitionsAPIKey = originalAPIKey
	})
}
