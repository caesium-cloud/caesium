package run

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestRetrySinglePartitionUsesKeyedLookupNotPageScan is the regression for the
// P1. `--partition` used to GET the listing endpoint with no selector and scan
// the response for a matching value. That endpoint is paginated (default page
// 100), so any instance past page one was reported as `partition "x" not
// found` — the bigger the fan-out, the more of the group was unreachable.
//
// The fixture makes the old behavior fail loudly: the unkeyed listing serves
// only instances 0..99, and the target lives at index 137.
func TestRetrySinglePartitionUsesKeyedLookupNotPageScan(t *testing.T) {
	restoreRetryTestGlobals(t)

	const (
		jobID  = "job-1"
		runID  = "run-1"
		task   = "process-file"
		target = "p137"
	)
	var sawUnkeyedList bool
	var lookupQuery string
	var retriedIndex string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/"+jobID+"/runs/"+runID+"/tasks/"+task+"/partitions":
			if value := r.URL.Query().Get("partition"); value != "" {
				lookupQuery = value
				if value != target {
					http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
					return
				}
				_, _ = fmt.Fprintf(w,
					`{"partitions":[{"value":%q,"index":137,"status":"failed","attempt":1,"cache_hit":false,"task_run_id":"tr-137"}],`+
						`"total":1,"limit":1,"offset":0,"next_offset":null}`, target)
				return
			}
			// The old code path: an unkeyed list. Serve only page one, exactly
			// as the server does.
			sawUnkeyedList = true
			rows := make([]string, 0, 100)
			for i := 0; i < 100; i++ {
				rows = append(rows, fmt.Sprintf(`{"value":"p%d","index":%d,"status":"succeeded"}`, i, i))
			}
			_, _ = fmt.Fprintf(w, `{"partitions":[%s],"total":200,"limit":100,"offset":0,"next_offset":100}`,
				strings.Join(rows, ","))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/retry"):
			parts := strings.Split(r.URL.Path, "/")
			retriedIndex = parts[len(parts)-2]
			_, _ = fmt.Fprintf(w, `{"retried":true,"index":137,"value":%q}`, target)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	retryFromFailureServer = server.URL
	retryFromFailureAPIKey = ""

	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	require.NoError(t, retrySinglePartition(context.Background(), cmd, jobID, runID, task, target))
	require.Equal(t, target, lookupQuery, "--partition must resolve through the keyed lookup")
	require.False(t, sawUnkeyedList, "--partition must not list-and-scan; a paginated list truncates the group")
	require.Equal(t, "137", retriedIndex, "the retry must target the index the keyed lookup returned")
	require.Contains(t, stdout.String(), "Retried partition")
}

// TestRetrySinglePartitionAcceptsBareInstanceLookupResponse keeps the CLI
// decoupled from which of the two documented keyed-lookup shapes the server
// settled on (`{"partitions":[one]}` vs. the bare instance object).
func TestRetrySinglePartitionAcceptsBareInstanceLookupResponse(t *testing.T) {
	restoreRetryTestGlobals(t)

	const (
		jobID = "job-1"
		runID = "run-1"
		task  = "process-file"
	)
	var retriedIndex string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			require.Equal(t, "charlie", r.URL.Query().Get("partition"))
			_, _ = w.Write([]byte(`{"value":"charlie","index":2,"status":"failed","task_run_id":"tr-2"}`))
		case http.MethodPost:
			parts := strings.Split(r.URL.Path, "/")
			retriedIndex = parts[len(parts)-2]
			_, _ = w.Write([]byte(`{"retried":true,"index":2,"value":"charlie"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	retryFromFailureServer = server.URL
	retryFromFailureAPIKey = ""

	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	require.NoError(t, retrySinglePartition(context.Background(), cmd, jobID, runID, task, "charlie"))
	require.Equal(t, "2", retriedIndex)
	require.Contains(t, stdout.String(), `Retried partition "charlie" (index 2)`)
}

// TestRetrySinglePartitionReportsKeyedLookup404 pins that an unknown partition
// surfaces as a readable CLI error rather than a raw HTTP dump.
func TestRetrySinglePartitionReportsKeyedLookup404(t *testing.T) {
	restoreRetryTestGlobals(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"task has no partition \"nope\" in this run"}`, http.StatusNotFound)
	}))
	defer server.Close()

	retryFromFailureServer = server.URL
	retryFromFailureAPIKey = ""

	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := retrySinglePartition(context.Background(), cmd, "job-1", "run-1", "process-file", "nope")
	require.Error(t, err)
	require.Contains(t, err.Error(), `partition "nope" not found`)
}

// executeRunCommand drives the REAL `run` command tree — cmd/run.Cmd and the
// subcommands registered on it — so cobra's own error and usage machinery is
// what the assertions observe. Calling a RunE directly would bypass exactly the
// code under test.
//
// This works because Cmd.Execute() runs on Root(), and in THIS test binary Cmd
// has no parent: the root `caesium` command lives in package cmd (cmd/execute.go),
// which package run does not import, so that wiring never runs here and Cmd is
// itself the root. The assertion below pins that, since a future import cycle
// would otherwise silently turn this into an os.Args parse.
//
// Note both streams are returned separately AND checked: cobra writes the error
// with PrintErrln (stderr) but the usage block with Println, which resolves to
// OutOrStderr() — so once a caller sets the out writer, usage lands on STDOUT.
// A merged capture would hide that, and it is precisely what would corrupt
// --json.
func executeRunCommand(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	require.False(t, Cmd.HasParent(),
		"cmd/run.Cmd gained a parent; Execute() would run on the real root and parse os.Args")

	var out, errOut bytes.Buffer
	Cmd.SetOut(&out)
	Cmd.SetErr(&errOut)
	Cmd.SetArgs(args)
	t.Cleanup(func() {
		Cmd.SetOut(nil)
		Cmd.SetErr(nil)
		Cmd.SetArgs(nil)
		// SilenceUsage is flipped by the RunE under test and would otherwise
		// persist on the shared command object, making a later "usage IS still
		// printed" assertion pass vacuously.
		for _, sub := range Cmd.Commands() {
			sub.SilenceUsage = false
		}
	})

	err = Cmd.Execute()
	return out.String(), errOut.String(), err
}

// TestRetrySinglePartitionServerErrorPrintsNoUsageBlock is the regression for
// the nit: a server-side refusal printed
//
//	Error: retry partition: 409 Conflict: {...}
//
//	Usage: ...
//
// and the forty lines of flag help buried the one sentence that told the
// operator what to do. A non-2xx response is not a usage error.
func TestRetrySinglePartitionServerErrorPrintsNoUsageBlock(t *testing.T) {
	restoreRetryTestGlobals(t)

	const conflict = "per-partition retry requires distributed execution mode " +
		"(CAESIUM_EXECUTION_MODE=distributed); in local mode nothing dispatches the reset " +
		"instance — retry the run instead"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			require.Equal(t, "e", r.URL.Query().Get("partition"))
			_, _ = w.Write([]byte(
				`{"partitions":[{"value":"e","index":4,"status":"failed"}],"total":1,"next_offset":null}`))
		case http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = fmt.Fprintf(w, `{"message":%q}`, conflict)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	stdout, stderr, err := executeRunCommand(t, "retry",
		"--job-id", "6f1c9d6e-0000-4000-8000-000000000001",
		"--run-id", "6f1c9d6e-0000-4000-8000-000000000002",
		"--task", "process", "--partition", "e", "--server", server.URL)

	require.Error(t, err)
	require.Contains(t, stderr, "409 Conflict")
	require.Contains(t, stderr, "distributed execution mode")
	require.Equal(t, 1, strings.Count(stderr, "distributed execution mode"),
		"the server's message must be printed exactly once:\n%s", stderr)

	// Neither stream may carry the usage block: cobra would write it to the out
	// writer here, which is the stdout a --json caller parses.
	require.NotContains(t, stderr, "Usage:", "usage must not follow a server error:\n%s", stderr)
	require.NotContains(t, stdout, "Usage:", "usage must never reach stdout:\n%s", stdout)
	require.Empty(t, stdout, "a failed retry writes nothing to stdout:\n%s", stdout)
}

// TestRetryStillPrintsUsageForArgumentErrors is the non-vacuity half: silencing
// usage must be scoped to server/store failures, not blanket-applied to the
// command. A missing --task with --partition is a usage error and still earns
// the usage block.
func TestRetryStillPrintsUsageForArgumentErrors(t *testing.T) {
	restoreRetryTestGlobals(t)

	stdout, stderr, err := executeRunCommand(t, "retry",
		"--job-id", "6f1c9d6e-0000-4000-8000-000000000001",
		"--run-id", "6f1c9d6e-0000-4000-8000-000000000002",
		"--partition", "e", "--server", "http://127.0.0.1:1")

	require.Error(t, err)
	require.Contains(t, err.Error(), "--task is required with --partition")
	require.Contains(t, stdout+stderr, "Usage:",
		"an argument error must still show usage:\nstdout=%s\nstderr=%s", stdout, stderr)
}
