package test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/harness"
	"github.com/caesium-cloud/caesium/internal/offlinereport"
	"github.com/stretchr/testify/require"
)

func TestSchemaJSONReportsSuccessAndFailure(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.job.yaml")
	bad := filepath.Join(dir, "bad.job.yaml")
	require.NoError(t, os.WriteFile(good, []byte(testJobYAML("schema-ok")), 0o644))
	require.NoError(t, os.WriteFile(bad, []byte(`apiVersion: v1
kind: Job
metadata:
  alias: schema-bad
trigger:
  type: cron
  configuration:
    cron: "*/5 * * * *"
steps: []
`), 0o644))

	t.Run("success", func(t *testing.T) {
		stdout, stderr, err := executeTest(t, "--json", "--path", good)
		require.NoError(t, err)
		require.Empty(t, stderr)
		require.NotContains(t, stdout, "PASS")
		report := decodeTestReport(t, stdout)
		require.Equal(t, 1, report.Version)
		require.Equal(t, "test", report.Command)
		require.True(t, report.OK)
		require.Equal(t, "schema", report.Scope.Kind)
		require.Equal(t, 0, report.Scope.Executed)
		require.Equal(t, 1, report.Discovered)
		require.Equal(t, 1, report.Selected)
		require.Equal(t, 1, report.Summary.Passed)
		require.Equal(t, "schema-ok", report.Definitions[0].Alias)
	})

	t.Run("failure keeps JSON on stdout", func(t *testing.T) {
		stdout, stderr, err := executeTest(t, "--json", "--path", dir)
		require.Error(t, err)
		require.NotContains(t, stderr, `"version"`)
		require.NotContains(t, stdout, "  FAIL")
		report := decodeTestReport(t, stdout)
		require.False(t, report.OK)
		require.Equal(t, "schema", report.Scope.Kind)
		require.GreaterOrEqual(t, report.Summary.Errors, 1)
		require.GreaterOrEqual(t, report.Discovered, 1)
		require.NotZero(t, report.Summary.Failed)
	})
}

func TestScenarioJSONReportsPassAndAssertionFailure(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "job.job.yaml"), []byte(testJobYAML("harness-json")), 0o644))
	scenario := filepath.Join(dir, "cases.scenario.yaml")
	require.NoError(t, os.WriteFile(scenario, []byte(`apiVersion: v1
kind: Harness
scenarios:
  - name: passes
    path: ./job.job.yaml
    expect:
      runStatus: succeeded
      tasks:
        - name: greet
          status: succeeded
  - name: fails
    path: ./job.job.yaml
    expect:
      runStatus: failed
      tasks:
        - name: greet
          status: failed
          output:
            code: "1"
`), 0o644))

	harness.SetTestEngineFactory(func(context.Context) atom.Engine { return &jsonFakeEngine{} })
	t.Cleanup(func() { harness.SetTestEngineFactory(nil) })

	stdout, stderr, err := executeTest(t, "--json", "--scenario", scenario)
	require.Error(t, err)
	require.NotContains(t, stderr, `"version"`)
	require.NotContains(t, stdout, "  PASS")
	require.NotContains(t, stdout, "  FAIL")
	report := decodeTestReport(t, stdout)
	require.Equal(t, 1, report.Version)
	require.Equal(t, "scenario", report.Scope.Kind)
	require.Equal(t, 2, report.Scope.Executed)
	require.Equal(t, 2, report.Discovered)
	require.Equal(t, 2, report.Selected)
	require.Len(t, report.Scenarios, 2)

	byName := map[string]offlinereport.Scenario{}
	for _, scenario := range report.Scenarios {
		byName[scenario.Name] = scenario
	}
	pass := byName["passes"]
	require.True(t, pass.Passed)
	require.NotNil(t, pass.Run)
	require.NotEmpty(t, pass.Run.RunID)
	require.NotEmpty(t, pass.Run.JobID)
	require.NotEmpty(t, pass.Run.Tasks)
	require.NotEmpty(t, pass.Run.Tasks[0].TaskID)
	require.Equal(t, "greet", pass.Run.Tasks[0].Name)
	require.Equal(t, "succeeded", pass.Run.Tasks[0].Status)

	fail := byName["fails"]
	require.False(t, fail.Passed)
	require.NotEmpty(t, fail.Assertions)
	foundStatus := false
	foundOutput := false
	for _, assertion := range fail.Assertions {
		require.Contains(t, assertionJSONKeys(t, assertion), "expected")
		require.Contains(t, assertionJSONKeys(t, assertion), "actual")
		if assertion.Name == "run status" {
			foundStatus = true
			require.Equal(t, "failed", assertion.Expected)
			require.Equal(t, "succeeded", assertion.Actual)
		}
		if assertion.Name == "task greet output[code]" {
			foundOutput = true
			require.Equal(t, "1", assertion.Expected)
			require.Equal(t, "", assertion.Actual)
		}
	}
	require.True(t, foundStatus, "missing run status assertion: %#v", fail.Assertions)
	require.True(t, foundOutput, "missing output assertion: %#v", fail.Assertions)
}

func executeTest(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	origPaths := append([]string(nil), testPaths...)
	origScenarios := append([]string(nil), scenarioPaths...)
	origImages := checkImages
	origVerbose := verboseOutput
	origJSON := jsonOutput
	origUsage := Cmd.SilenceUsage
	t.Cleanup(func() {
		testPaths = origPaths
		scenarioPaths = origScenarios
		checkImages = origImages
		verboseOutput = origVerbose
		jsonOutput = origJSON
		Cmd.SilenceUsage = origUsage
		for _, name := range []string{"path", "scenario", "check-images", "verbose", "json"} {
			flag := Cmd.Flags().Lookup(name)
			if flag == nil {
				continue
			}
			_ = flag.Value.Set(flag.DefValue)
			flag.Changed = false
		}
		testPaths = origPaths
		scenarioPaths = origScenarios
		jsonOutput = origJSON
		checkImages = origImages
		Cmd.SetOut(nil)
		Cmd.SetErr(nil)
		Cmd.SetArgs(nil)
	})

	var stdout, stderr strings.Builder
	Cmd.SetOut(&stdout)
	Cmd.SetErr(&stderr)
	Cmd.SetArgs(args)
	err := Cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func decodeTestReport(t *testing.T, stdout string) offlinereport.Report {
	t.Helper()
	require.True(t, json.Valid([]byte(stdout)), "stdout was not JSON:\n%s", stdout)
	var report offlinereport.Report
	require.NoError(t, json.Unmarshal([]byte(stdout), &report))
	return report
}

func assertionJSONKeys(t *testing.T, assertion offlinereport.Assertion) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(assertion)
	require.NoError(t, err)
	var keys map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &keys))
	return keys
}

func testJobYAML(alias string) string {
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

type jsonFakeEngine struct {
	mu sync.Mutex
}

type jsonFakeAtom struct{ id string }

func (a *jsonFakeAtom) ID() string           { return a.id }
func (a *jsonFakeAtom) State() atom.State    { return atom.Stopped }
func (a *jsonFakeAtom) Result() atom.Result  { return atom.Success }
func (a *jsonFakeAtom) ExitCode() *int       { return nil }
func (a *jsonFakeAtom) CreatedAt() time.Time { return time.Now() }
func (a *jsonFakeAtom) StartedAt() time.Time { return time.Now() }
func (a *jsonFakeAtom) StoppedAt() time.Time { return time.Now() }
func (a *jsonFakeAtom) Engine() atom.Engine  { return nil }

func (e *jsonFakeEngine) Create(req *atom.EngineCreateRequest) (atom.Atom, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return &jsonFakeAtom{id: "atom-" + req.Name}, nil
}

func (e *jsonFakeEngine) Get(req *atom.EngineGetRequest) (atom.Atom, error) {
	return &jsonFakeAtom{id: req.ID}, nil
}

func (e *jsonFakeEngine) List(*atom.EngineListRequest) ([]atom.Atom, error) { return nil, nil }

func (e *jsonFakeEngine) Wait(req *atom.EngineWaitRequest) (atom.Atom, error) {
	return &jsonFakeAtom{id: req.ID}, nil
}

func (e *jsonFakeEngine) Stop(*atom.EngineStopRequest) error { return nil }

func (e *jsonFakeEngine) Logs(*atom.EngineLogsRequest) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (e *jsonFakeEngine) Stats(*atom.EngineStatsRequest) (atom.ResourceStats, error) {
	return atom.ResourceStats{}, atom.ErrStatsUnavailable
}
