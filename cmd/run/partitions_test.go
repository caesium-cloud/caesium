package run

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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
	originalLimit := partitionsLimit
	originalOffset := partitionsOffset
	originalServer := partitionsServer
	originalAPIKey := partitionsAPIKey
	t.Cleanup(func() {
		partitionsJobID = originalJobID
		partitionsTask = originalTask
		partitionsStatus = originalStatus
		partitionsJSON = originalJSON
		partitionsLimit = originalLimit
		partitionsOffset = originalOffset
		partitionsServer = originalServer
		partitionsAPIKey = originalAPIKey
	})
}

// withPartitionsPagingFlags registers --limit/--offset on a bare test command so
// runPartitions' `Flags().Changed(...)` check can see them. The single-page mode
// is selected by the flag being CHANGED, not by its value, so a test that only
// assigned the package globals would still exercise the full-walk path.
func withPartitionsPagingFlags(t *testing.T, cmd *cobra.Command, limit, offset int) {
	t.Helper()
	cmd.Flags().IntVar(&partitionsLimit, "limit", 0, "")
	cmd.Flags().IntVar(&partitionsOffset, "offset", 0, "")
	require.NoError(t, cmd.Flags().Set("limit", strconv.Itoa(limit)))
	if offset > 0 {
		require.NoError(t, cmd.Flags().Set("offset", strconv.Itoa(offset)))
	}
}

// pagedPartitionsServer serves `total` synthetic instances through the paginated
// envelope, honoring limit/offset and reporting next_offset exactly as the API
// contract specifies (null on the last page). It records every offset it was
// asked for so a test can prove the CLI actually walked.
func pagedPartitionsServer(t *testing.T, total int, offsets *[]int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 1000 {
			limit = 100
		}
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		*offsets = append(*offsets, offset)

		rows := make([]string, 0, limit)
		for i := offset; i < offset+limit && i < total; i++ {
			rows = append(rows, fmt.Sprintf(
				`{"value":"p%02d","index":%d,"status":"succeeded","attempt":1,"cache_hit":false,"task_run_id":"tr-%02d"}`,
				i, i, i))
		}
		nextOffset := "null"
		if offset+len(rows) < total {
			nextOffset = strconv.Itoa(offset + len(rows))
		}
		_, _ = fmt.Fprintf(w,
			`{"partitions":[%s],"total":%d,"limit":%d,"offset":%d,"next_offset":%s,"status_counts":{"succeeded":%d}}`,
			strings.Join(rows, ","), total, limit, offset, nextOffset, total)
	}))
}

// TestRunPartitionsPagesToCompletion is the regression for the P1: the listing
// endpoint is paginated (default page 100), and the CLI used to read ONE
// response and render it as the group. A 250-instance group therefore printed
// 100 rows with nothing saying so.
func TestRunPartitionsPagesToCompletion(t *testing.T) {
	restorePartitionsTestGlobals(t)

	var offsets []int
	server := pagedPartitionsServer(t, 1250, &offsets)
	defer server.Close()

	partitionsJobID = "job-1"
	partitionsTask = "process-file"
	partitionsServer = server.URL
	partitionsAPIKey = ""
	partitionsJSON = false

	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	require.NoError(t, runPartitions(cmd, []string{"run-1"}))

	// 1250 rows at the CLI's 500-row page size is three requests.
	require.Equal(t, []int{0, 500, 1000}, offsets, "the CLI must follow next_offset to the end of the list")

	out := stdout.String()
	require.Contains(t, out, "1250 partitions", "the table must report the whole group, not one page")
	require.NotContains(t, out, "Showing ", "a completed walk is not a truncated view")
	// First, last, and a row that only exists on the third page.
	require.Contains(t, out, "p00")
	require.Contains(t, out, "p1249")
	require.Contains(t, out, "p1100")
	require.Empty(t, stderr.String(), "a completed walk emits no truncation note")
}

// TestRunPartitionsJSONAggregatesEveryPage pins that --json emits the FULL list,
// not page one's envelope: a scripted caller that trusted the old output was
// silently reading a truncated group.
func TestRunPartitionsJSONAggregatesEveryPage(t *testing.T) {
	restorePartitionsTestGlobals(t)

	var offsets []int
	server := pagedPartitionsServer(t, 700, &offsets)
	defer server.Close()

	partitionsJobID = "job-1"
	partitionsTask = "process-file"
	partitionsServer = server.URL
	partitionsAPIKey = ""
	partitionsJSON = true

	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	require.NoError(t, runPartitions(cmd, []string{"run-1"}))
	require.Equal(t, []int{0, 500}, offsets)

	var doc struct {
		Partitions []struct {
			Value     string `json:"value"`
			TaskRunID string `json:"task_run_id"`
		} `json:"partitions"`
		Total        int            `json:"total"`
		StatusCounts map[string]int `json:"status_counts"`
		NextOffset   *int           `json:"next_offset"`
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &doc), "stdout was not valid JSON:\n%s", stdout.String())
	require.Len(t, doc.Partitions, 700, "--json must carry every page's rows")
	require.Equal(t, 700, doc.Total)
	require.Equal(t, map[string]int{"succeeded": 700}, doc.StatusCounts)
	require.Nil(t, doc.NextOffset, "a completed walk has no continuation cursor")
	// Fields the CLI's own row struct does not model must survive verbatim.
	require.Equal(t, "tr-00", doc.Partitions[0].TaskRunID)
	require.Equal(t, "p699", doc.Partitions[699].Value)
	require.Empty(t, stderr.String())
}

// TestRunPartitionsExplicitLimitFetchesOnePageAndNotesTruncation covers the
// escape hatch: an explicit --limit hands paging to the caller, so exactly one
// request is made, the continuation cursor is reported, and the "this is a
// window" note goes to STDERR so --json stays parseable.
func TestRunPartitionsExplicitLimitFetchesOnePageAndNotesTruncation(t *testing.T) {
	restorePartitionsTestGlobals(t)

	var offsets []int
	server := pagedPartitionsServer(t, 40, &offsets)
	defer server.Close()

	partitionsJobID = "job-1"
	partitionsTask = "process-file"
	partitionsServer = server.URL
	partitionsAPIKey = ""
	partitionsJSON = true

	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	withPartitionsPagingFlags(t, cmd, 10, 20)

	require.NoError(t, runPartitions(cmd, []string{"run-1"}))
	require.Equal(t, []int{20}, offsets, "--limit/--offset must issue exactly one request at the given offset")

	var doc struct {
		Partitions []struct {
			Value string `json:"value"`
		} `json:"partitions"`
		Total      int  `json:"total"`
		NextOffset *int `json:"next_offset"`
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &doc), "stdout was not valid JSON:\n%s", stdout.String())
	require.Len(t, doc.Partitions, 10)
	require.Equal(t, "p20", doc.Partitions[0].Value)
	require.Equal(t, 40, doc.Total)
	require.NotNil(t, doc.NextOffset)
	require.Equal(t, 30, *doc.NextOffset)

	require.Contains(t, stderr.String(), "showing 10 of 40 partitions")
	require.Contains(t, stderr.String(), "--offset 30")
}

// TestRunPartitionsTableShowsWindowWhenExplicitlyPaged pins the human-readable
// counterpart: a windowed table has to say it is a window.
func TestRunPartitionsTableShowsWindowWhenExplicitlyPaged(t *testing.T) {
	restorePartitionsTestGlobals(t)

	var offsets []int
	server := pagedPartitionsServer(t, 40, &offsets)
	defer server.Close()

	partitionsJobID = "job-1"
	partitionsTask = "process-file"
	partitionsServer = server.URL
	partitionsAPIKey = ""
	partitionsJSON = false

	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	withPartitionsPagingFlags(t, cmd, 5, 0)

	require.NoError(t, runPartitions(cmd, []string{"run-1"}))
	require.Contains(t, stdout.String(), "Showing 5 of 40 partitions")
	require.Contains(t, stderr.String(), "continue with --offset 5")
}

// TestRunPartitionsStopsOnNonAdvancingCursor guards the walk against a server
// that echoes a next_offset which never moves. Without the guard the CLI spins
// forever on a live cluster; with it, the walk ends and the count line makes the
// shortfall visible.
func TestRunPartitionsStopsOnNonAdvancingCursor(t *testing.T) {
	restorePartitionsTestGlobals(t)

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte(
			`{"partitions":[{"value":"alpha","index":0,"status":"succeeded","attempt":1,"cache_hit":false}],` +
				`"total":9,"limit":500,"offset":0,"next_offset":0}`))
	}))
	defer server.Close()

	partitionsJobID = "job-1"
	partitionsTask = "process-file"
	partitionsServer = server.URL
	partitionsAPIKey = ""
	partitionsJSON = false

	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	require.NoError(t, runPartitions(cmd, []string{"run-1"}))
	require.Equal(t, 1, requests, "a cursor that does not advance must end the walk, not restart it")
	require.Contains(t, stdout.String(), "Showing 1 of 9 partitions")
}

// TestRunPartitionsPreAPaginationServerReadsAsOnePage pins backward
// compatibility: a server that sends neither next_offset nor total (the shape
// before pagination landed) must render exactly as it used to.
func TestRunPartitionsPreAPaginationServerReadsAsOnePage(t *testing.T) {
	restorePartitionsTestGlobals(t)

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"partitions":[
			{"value":"alpha","index":0,"status":"succeeded","attempt":1,"cache_hit":false},
			{"value":"bravo","index":1,"status":"failed","attempt":2,"cache_hit":false,"error":"boom"}
		]}`))
	}))
	defer server.Close()

	partitionsJobID = "job-1"
	partitionsTask = "process-file"
	partitionsServer = server.URL
	partitionsAPIKey = ""
	partitionsJSON = false

	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	require.NoError(t, runPartitions(cmd, []string{"run-1"}))
	require.Equal(t, 1, requests)
	out := stdout.String()
	require.Contains(t, out, "2 partitions")
	require.Contains(t, out, "alpha")
	require.Contains(t, out, "bravo")
}

// TestRunPartitionsServerErrorPrintsNoUsageBlockAndKeepsStdoutClean pins the
// same convention on the list verb, and the stdout half specifically: cobra
// writes its usage block with Println, which resolves to OutOrStderr(), so an
// un-silenced usage block lands on STDOUT the moment the command's out writer
// is set — directly corrupting the --json document a caller is parsing.
func TestRunPartitionsServerErrorPrintsNoUsageBlockAndKeepsStdoutClean(t *testing.T) {
	restorePartitionsTestGlobals(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"internal server error"}`))
	}))
	defer server.Close()

	stdout, stderr, err := executeRunCommand(t, "partitions", "run-1",
		"--job-id", "job-1", "--task", "process", "--server", server.URL, "--json")

	require.Error(t, err)
	require.Contains(t, stderr, "partitions failed (500)")
	require.Contains(t, stderr, "internal server error")
	require.NotContains(t, stderr, "Usage:", "usage must not follow a server error:\n%s", stderr)
	require.NotContains(t, stdout, "Usage:", "usage must never reach stdout:\n%s", stdout)
	require.Empty(t, stdout, "--json stdout must stay clean on failure:\n%s", stdout)
}

// TestRunPartitionsStillPrintsUsageForArgumentErrors is the non-vacuity half.
func TestRunPartitionsStillPrintsUsageForArgumentErrors(t *testing.T) {
	restorePartitionsTestGlobals(t)

	partitionsJobID = ""
	partitionsTask = ""

	stdout, stderr, err := executeRunCommand(t, "partitions", "run-1",
		"--server", "http://127.0.0.1:1")

	require.Error(t, err)
	require.Contains(t, err.Error(), "--job-id and --task are required")
	require.Contains(t, stdout+stderr, "Usage:",
		"an argument error must still show usage:\nstdout=%s\nstderr=%s", stdout, stderr)
}
