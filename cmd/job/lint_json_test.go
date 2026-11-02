package job

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/offlinereport"
	"github.com/stretchr/testify/require"
)

func TestLintOfflineJSONSuccessAndFailure(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.job.yaml")
	bad := filepath.Join(dir, "bad.job.yaml")
	require.NoError(t, os.WriteFile(good, []byte(lintJobYAML("demo")), 0o644))
	require.NoError(t, os.WriteFile(bad, []byte(`apiVersion: v1
kind: Job
metadata:
  alias: broken
  schemaValidaton: fail
  notAField: true
trigger:
  type: cron
  configuration:
    cron: "*/5 * * * *"
steps:
  - name: greet
    image: alpine:3.23
    command: ["echo", "hi"]
`), 0o644))

	t.Run("success", func(t *testing.T) {
		stdout, stderr, err := executeLint(t, "--json", "--path", good)
		require.NoError(t, err)
		require.NotContains(t, stderr, `"version"`)
		require.NotContains(t, stdout, "Validated")
		report := decodeReport(t, stdout)
		require.Equal(t, 1, report.Version)
		require.Equal(t, "lint", report.Command)
		require.True(t, report.OK)
		require.Equal(t, 0, report.Summary.Errors)
		require.Equal(t, 1, report.Discovered)
		require.Equal(t, 1, report.Selected)
		require.Contains(t, report.Files, good)
		require.Equal(t, "lint", report.Scope.Kind)
	})

	t.Run("failure still prints JSON", func(t *testing.T) {
		stdout, stderr, err := executeLint(t, "--json", "--path", bad)
		require.Error(t, err)
		require.NotContains(t, stderr, `"version"`)
		require.NotContains(t, stderr, stdout)
		require.NotContains(t, stdout, "--json is only supported with --server")
		report := decodeReport(t, stdout)
		require.Equal(t, 1, report.Version)
		require.False(t, report.OK)
		require.GreaterOrEqual(t, report.Summary.Errors, 2)
		require.GreaterOrEqual(t, report.Discovered, 1)
		var paths []string
		for _, diag := range report.Diagnostics {
			if diag.Severity != offlinereport.SeverityError {
				continue
			}
			require.Equal(t, bad, diag.File)
			require.NotZero(t, diag.Line)
			require.NotEmpty(t, diag.Path)
			paths = append(paths, diag.Path)
		}
		require.Contains(t, paths, "metadata.schemaValidaton")
		require.Contains(t, paths, "metadata.notAField")
	})
}

func TestLintServerJSONKeepsServerDocument(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.job.yaml")
	require.NoError(t, os.WriteFile(good, []byte(lintJobYAML("server-demo")), 0o644))

	const body = `{"errors":[],"warnings":[],"summary":{"steps":1},"contracts":{"breaking":[],"warnings":[],"edges":0}}`
	var hits []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		switch r.URL.Path {
		case "/v1/system/features":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"contract_enforcement_enabled":true}`))
		case "/v1/jobdefs/lint":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	stdout, stderr, err := executeLint(t, "--json", "--server", server.URL, "--path", good)
	require.NoError(t, err)
	require.NotContains(t, stderr, stdout)
	require.Contains(t, hits, "/v1/jobdefs/lint")

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	require.Contains(t, got, "summary")
	require.Contains(t, got, "contracts")
	require.NotContains(t, got, "discovered")
	require.NotContains(t, got, "version")

	var want map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &want))
	require.Equal(t, want, got)
}

func executeLint(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	// Execute on a child walks to the parent and would ignore args set on lintCmd.
	require.False(t, Cmd.HasParent(), "job.Cmd gained a parent; Execute would parse os.Args")

	origPaths := append([]string(nil), lintPaths...)
	origJSON := lintJSON
	origServer := lintServer
	origSecrets := lintCheckSecrets
	origUsage := lintCmd.SilenceUsage
	t.Cleanup(func() {
		lintPaths = origPaths
		lintJSON = origJSON
		lintServer = origServer
		lintCheckSecrets = origSecrets
		lintCmd.SilenceUsage = origUsage
		for _, name := range []string{"path", "json", "server", "check-secrets"} {
			flag := lintCmd.Flags().Lookup(name)
			if flag == nil {
				continue
			}
			_ = flag.Value.Set(flag.DefValue)
			flag.Changed = false
		}
		lintPaths = origPaths
		lintServer = origServer
		lintJSON = origJSON
		Cmd.SetOut(nil)
		Cmd.SetErr(nil)
		Cmd.SetArgs(nil)
		lintCmd.SetOut(nil)
		lintCmd.SetErr(nil)
		lintCmd.SetArgs(nil)
	})

	var stdout, stderr strings.Builder
	Cmd.SetOut(&stdout)
	Cmd.SetErr(&stderr)
	Cmd.SetArgs(append([]string{"lint"}, args...))
	err := Cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func decodeReport(t *testing.T, stdout string) offlinereport.Report {
	t.Helper()
	require.True(t, json.Valid([]byte(stdout)), "stdout was not JSON:\n%s", stdout)
	var report offlinereport.Report
	require.NoError(t, json.Unmarshal([]byte(stdout), &report))
	return report
}

func lintJobYAML(alias string) string {
	return `apiVersion: v1
kind: Job
metadata:
  alias: ` + alias + `
trigger:
  type: cron
  configuration:
    cron: "*/5 * * * *"
steps:
  - name: greet
    image: alpine:3.23
    command: ["echo", "hi"]
`
}
