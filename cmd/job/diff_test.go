package job

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestDiffFlagsRegistered(t *testing.T) {
	for _, name := range []string{"server", "json", "prune", "api-key", "path"} {
		if diffCmd.Flags().Lookup(name) == nil {
			t.Fatalf("--%s flag is not registered", name)
		}
	}

	server := diffCmd.Flags().Lookup("server")
	if got := server.DefValue; got != defaultDiffServer {
		t.Fatalf("--server default = %q, want %q", got, defaultDiffServer)
	}
	if got := diffCmd.Flags().Lookup("json").DefValue; got != "false" {
		t.Fatalf("--json default = %q, want false", got)
	}
	if got := diffCmd.Flags().Lookup("prune").DefValue; got != "false" {
		t.Fatalf("--prune default = %q, want false", got)
	}
	if got := diffCmd.Flags().Lookup("api-key").DefValue; got != "" {
		t.Fatalf("--api-key default = %q, want empty", got)
	}
}

func TestDiffHelpDescribesServerNotDatabase(t *testing.T) {
	require.Contains(t, diffCmd.Short, "server")
	require.NotContains(t, diffCmd.Short, "database")
	require.Contains(t, diffCmd.Long, "POST /v1/jobdefs/diff")
	require.Contains(t, diffCmd.Long, "--prune")
	require.Contains(t, diffCmd.Long, "--json")
	require.Contains(t, diffCmd.Long, "Exit status")
	require.Contains(t, diffCmd.Long, "diffed fields")
	require.Contains(t, diffCmd.Long, "JobSpec")
	require.Contains(t, diffCmd.Long, "byte-equal apply")
}

func TestRejectDuplicateAliases(t *testing.T) {
	unique := []schema.Definition{
		{Metadata: schema.Metadata{Alias: "a"}},
		{Metadata: schema.Metadata{Alias: "b"}},
	}
	require.NoError(t, rejectDuplicateAliases(unique))
	require.NoError(t, rejectDuplicateAliases(nil))

	err := rejectDuplicateAliases([]schema.Definition{
		{Metadata: schema.Metadata{Alias: "dup-job"}},
		{Metadata: schema.Metadata{Alias: "other"}},
		{Metadata: schema.Metadata{Alias: "dup-job"}},
	})
	require.EqualError(t, err, `duplicate job alias "dup-job"`)
}

func TestDiffRejectsDuplicateAliasesWithoutHittingServer(t *testing.T) {
	const alias = "dup-job"
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "one.yaml"), []byte(diffTestManifest(alias, "echo first")), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "two.yaml"), []byte(diffTestManifest(alias, "echo second")), 0o644))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("job diff must reject duplicate aliases before POST, got %s %s", r.Method, r.URL.Path)
		http.Error(w, "server should not be called", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	_, _, err := executeDiffCommand(t, "--path", dir, "--server", srv.URL)
	require.EqualError(t, err, `duplicate job alias "dup-job"`)
}

func TestScopeJobDiffOmitsRemovesWithoutPrune(t *testing.T) {
	resp := &jobDiffResponse{
		Added:    []json.RawMessage{[]byte(`{"alias":"new-job"}`)},
		Removed:  []json.RawMessage{[]byte(`{"alias":"other-job"}`)},
		Modified: []json.RawMessage{[]byte(`{"alias":"changed-job","diff":"-old\n+new\n"}`)},
	}

	scoped := scopeJobDiff(resp, false)
	require.True(t, scoped.inScope())
	require.Equal(t, []string{"new-job"}, rawAliases(scoped.Added))
	require.Equal(t, []string{"changed-job"}, rawAliases(scoped.Modified))
	require.Empty(t, scoped.Removed)
	require.Equal(t, []string{"other-job"}, rawAliases(scoped.WouldPrune))

	pruned := scopeJobDiff(resp, true)
	require.True(t, pruned.inScope())
	require.Equal(t, []string{"other-job"}, rawAliases(pruned.Removed))
	require.Empty(t, pruned.WouldPrune)
}

func TestScopeJobDiffUnrelatedServerJobsAreNotInScope(t *testing.T) {
	resp := &jobDiffResponse{
		Removed: []json.RawMessage{
			[]byte(`{"alias":"already-on-server"}`),
			[]byte(`{"alias":"another-job"}`),
		},
	}

	scoped := scopeJobDiff(resp, false)
	require.False(t, scoped.inScope())
	require.Empty(t, scoped.Added)
	require.Empty(t, scoped.Modified)
	require.Empty(t, scoped.Removed)
	require.Equal(t, []string{"already-on-server", "another-job"}, rawAliases(scoped.WouldPrune))
	require.NoError(t, jobDiffInScopeError(scoped))

	pruned := scopeJobDiff(resp, true)
	require.True(t, pruned.inScope())
	err := jobDiffInScopeError(pruned)
	require.Error(t, err)
	require.ErrorIs(t, err, errJobDiffInScope)
	require.Contains(t, err.Error(), "2 to delete")
}

func TestJobDiffInScopeErrorCountsCreatesAndUpdates(t *testing.T) {
	scoped := scopedJobDiff{
		Added:    []json.RawMessage{[]byte(`{"alias":"a"}`)},
		Modified: []json.RawMessage{[]byte(`{"alias":"b"}`), []byte(`{"alias":"c"}`)},
	}
	err := jobDiffInScopeError(scoped)
	require.ErrorIs(t, err, errJobDiffInScope)
	require.Contains(t, err.Error(), "1 to create")
	require.Contains(t, err.Error(), "2 to update")
	require.True(t, errors.Is(err, errJobDiffInScope))
}

func TestRenderJobDiffSeparatesPruneCandidates(t *testing.T) {
	cmd, out := testDiffCmd()
	scoped := scopeJobDiff(&jobDiffResponse{
		Added:   []json.RawMessage{[]byte(`{"alias":"new-job"}`)},
		Removed: []json.RawMessage{[]byte(`{"alias":"other-job"}`)},
	}, false)

	require.NoError(t, renderJobDiff(cmd, scoped))
	got := out.String()
	require.Contains(t, got, "Creates:")
	require.Contains(t, got, "  - new-job")
	require.NotContains(t, got, "Deletes:")
	require.Contains(t, got, jobDiffWouldPruneHdr)
	require.Contains(t, got, "  - other-job")
}

func TestRenderJobDiffPrintsContractFindings(t *testing.T) {
	cmd, out := testDiffCmd()
	modified := []byte(`{
		"alias":"producer",
		"diff":"- required: [customer_id]\n+ required: [row_count]\n",
		"contractFindings":[{
			"edgeClass":"inferred",
			"from":"job:producer",
			"to":"job:consumer",
			"kind":"requirement_unsatisfied",
			"path":"trigger.configuration.paramMapping.customer",
			"key":"customer_id",
			"detail":"missing customer_id",
			"verdict":"breaking"
		}]
	}`)
	created := []byte(`{
		"alias":"new-job",
		"contractFindings":[{
			"from":"job:new-job",
			"to":"job:downstream",
			"edgeClass":"inferred",
			"kind":"requirement_unsatisfied",
			"path":"steps[0].datasets.consumes",
			"key":"row_count",
			"detail":"missing row_count",
			"verdict":"warning"
		}]
	}`)
	removed := []byte(`{
		"alias":"gone",
		"contractFindings":[{
			"from":"job:gone",
			"to":"job:consumer",
			"key":"customer_id",
			"verdict":"breaking",
			"detail":"producer removed"
		}]
	}`)

	require.NoError(t, renderJobDiff(cmd, scopedJobDiff{
		Added:    []json.RawMessage{created},
		Modified: []json.RawMessage{modified},
		Removed:  []json.RawMessage{removed},
	}))
	got := out.String()
	require.Contains(t, got, "Creates:")
	require.Contains(t, got, "  - new-job")
	require.Contains(t, got, "warning: job:new-job -> job:downstream [inferred]")
	require.Contains(t, got, "row_count")
	require.Contains(t, got, "Updates:")
	require.Contains(t, got, "  - producer")
	require.Contains(t, got, "breaking: job:producer -> job:consumer [inferred] requirement_unsatisfied trigger.configuration.paramMapping.customer customer_id: missing customer_id")
	require.Contains(t, got, "Deletes:")
	require.Contains(t, got, "  - gone")
	require.Contains(t, got, "breaking:")
	require.Contains(t, got, "job:consumer")
	require.Contains(t, got, "customer_id")
}

func TestDiffPrintsContractFindingsFromServer(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "job.yaml"), []byte(diffTestManifest("producer", "echo producer")), 0o644))

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/jobdefs/diff", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"added":[],
			"removed":[],
			"modified":[{
				"alias":"producer",
				"diff":"- required: [customer_id, row_count]\n+ required: [row_count]\n",
				"contractFindings":[{
					"edgeId":"inferred:producer:consumer",
					"edgeClass":"inferred",
					"from":"job:producer",
					"to":"job:consumer",
					"kind":"requirement_unsatisfied",
					"path":"trigger.configuration.paramMapping.customer",
					"key":"customer_id",
					"detail":"missing customer_id",
					"verdict":"breaking"
				}]
			}]
		}`))
	}))
	t.Cleanup(srv.Close)

	stdout, stderr, err := executeDiffCommand(t, "--path", dir, "--server", srv.URL)
	require.Error(t, err)
	require.ErrorIs(t, err, errJobDiffInScope)
	require.Equal(t, 1, hits, "job diff must POST to the dummy server")
	require.Contains(t, stdout, "Updates:")
	require.Contains(t, stdout, "producer")
	require.Contains(t, stdout, "job:consumer")
	require.Contains(t, stdout, "customer_id")
	require.Contains(t, stdout, "breaking")
	require.NotContains(t, stderr, `"contractFindings"`)
}

func TestRenderJobDiffPruneListsDeletes(t *testing.T) {
	cmd, out := testDiffCmd()
	scoped := scopeJobDiff(&jobDiffResponse{
		Removed: []json.RawMessage{[]byte(`{"alias":"gone"}`)},
	}, true)

	require.NoError(t, renderJobDiff(cmd, scoped))
	got := out.String()
	require.Contains(t, got, "Deletes:")
	require.Contains(t, got, "  - gone")
	require.NotContains(t, got, jobDiffWouldPruneHdr)
}

func TestRenderJobDiffNoChanges(t *testing.T) {
	cmd, out := testDiffCmd()
	require.NoError(t, renderJobDiff(cmd, scopedJobDiff{}))
	require.Equal(t, "No changes detected.\n", out.String())
}

func TestRenderJobDiffUnchangedWithPruneCandidates(t *testing.T) {
	cmd, out := testDiffCmd()
	scoped := scopeJobDiff(&jobDiffResponse{
		Removed: []json.RawMessage{[]byte(`{"alias":"other"}`)},
	}, false)
	require.NoError(t, renderJobDiff(cmd, scoped))
	got := out.String()
	require.Contains(t, got, "No changes detected.")
	require.Contains(t, got, jobDiffWouldPruneHdr)
	require.Contains(t, got, "  - other")
	require.NotContains(t, got, "Deletes:")
}

func TestWriteJobDiffJSONOmitsRemovesWithoutPrune(t *testing.T) {
	cmd, out := testDiffCmd()
	scoped := scopeJobDiff(&jobDiffResponse{
		Added:   []json.RawMessage{[]byte(`{"alias":"new-job","labels":{"k":"v"}}`)},
		Removed: []json.RawMessage{[]byte(`{"alias":"other-job"}`)},
	}, false)

	require.NoError(t, writeJobDiffJSON(cmd, scoped))
	raw := out.Bytes()
	require.True(t, json.Valid(bytes.TrimSpace(raw)), "stdout: %s", raw)

	var parsed jobDiffJSON
	require.NoError(t, json.Unmarshal(raw, &parsed))
	require.Equal(t, jobDiffJSONVersion, parsed.Version)
	require.Equal(t, []string{"new-job"}, rawAliases(parsed.Added))
	require.Contains(t, string(parsed.Added[0]), `"labels"`)
	require.Empty(t, parsed.Removed)
	require.Equal(t, []string{"other-job"}, rawAliases(parsed.WouldPrune))
}

func TestSendDiffRequestPostsJSONAndBearerToken(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotAuth   string
		gotCT     string
		gotBody   jobDiffRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := json.Unmarshal(body, &gotBody); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"added":[{"alias":"n"}],"removed":[{"alias":"o"}],"modified":[]}`))
	}))
	defer srv.Close()

	defs := []schema.Definition{{
		APIVersion: schema.APIVersionV1,
		Kind:       schema.KindJob,
		Metadata:   schema.Metadata{Alias: "n"},
	}}
	resp, err := sendDiffRequest(context.Background(), srv.URL, "secret-key", defs)
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, gotMethod)
	require.Equal(t, "/v1/jobdefs/diff", gotPath)
	require.Equal(t, "Bearer secret-key", gotAuth)
	require.Equal(t, "application/json", gotCT)
	require.Len(t, gotBody.Definitions, 1)
	require.Equal(t, "n", gotBody.Definitions[0].Metadata.Alias)
	require.Equal(t, []string{"n"}, rawAliases(resp.Added))
	require.Equal(t, []string{"o"}, rawAliases(resp.Removed))
}

func TestSendDiffRequestSurfacesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request: invalid definition", http.StatusBadRequest)
	}))
	defer srv.Close()

	_, err := sendDiffRequest(context.Background(), srv.URL, "", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "job diff failed (400)")
	require.Contains(t, err.Error(), "invalid definition")
}

func TestWriteJobDiffJSONPrunePutsDeletesInRemoved(t *testing.T) {
	cmd, out := testDiffCmd()
	scoped := scopeJobDiff(&jobDiffResponse{
		Removed: []json.RawMessage{[]byte(`{"alias":"other-job"}`)},
	}, true)

	require.NoError(t, writeJobDiffJSON(cmd, scoped))
	var parsed jobDiffJSON
	require.NoError(t, json.Unmarshal(out.Bytes(), &parsed))
	require.Equal(t, []string{"other-job"}, rawAliases(parsed.Removed))
	require.Empty(t, parsed.WouldPrune)
}

func testDiffCmd() (*cobra.Command, *bytes.Buffer) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	return cmd, &out
}

func executeDiffCommand(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	require.False(t, Cmd.HasParent(),
		"cmd/job.Cmd gained a parent; Execute() would run on the real root and parse os.Args")

	prevPaths := append([]string(nil), diffPaths...)
	prevServer, prevAPIKey, prevJSON, prevPrune := diffServer, diffAPIKey, diffJSON, diffPrune
	t.Cleanup(func() {
		diffPaths, diffServer, diffAPIKey, diffJSON, diffPrune = prevPaths, prevServer, prevAPIKey, prevJSON, prevPrune
		if f := diffCmd.Flags().Lookup("path"); f != nil {
			f.Changed = false
		}
		diffCmd.SilenceUsage = false
		diffCmd.SilenceErrors = false
		Cmd.SetOut(nil)
		Cmd.SetErr(nil)
		Cmd.SetArgs(nil)
	})

	var out, errOut bytes.Buffer
	Cmd.SetOut(&out)
	Cmd.SetErr(&errOut)
	Cmd.SetArgs(append([]string{"diff"}, args...))
	err = Cmd.Execute()
	return out.String(), errOut.String(), err
}

func diffTestManifest(alias, command string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    cron: "0 0 1 1 *"
steps:
  - name: run
    image: alpine:3.23
    command: ["sh", "-c", %q]
`, alias, command)
}

func rawAliases(items []json.RawMessage) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, rawAlias(item))
	}
	return out
}
