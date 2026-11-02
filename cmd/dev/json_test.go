package dev

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/offlinereport"
	"github.com/stretchr/testify/require"
)

func TestDevJSONRequiresOnce(t *testing.T) {
	stdout, stderr, err := executeDev(t, "--json", "--path", t.TempDir())
	require.EqualError(t, err, "--json requires --once; watch mode does not stream JSON")
	require.Empty(t, stdout)
	require.NotContains(t, stderr, `"version"`)
	require.NotContains(t, stdout, "{")
}

func TestDevOnceJSONReportsTheLocalRun(t *testing.T) {
	dir := t.TempDir()
	job := filepath.Join(dir, "job.job.yaml")
	require.NoError(t, os.WriteFile(job, []byte(testManifest("dev-json")), 0o644))
	testEngineFactory = func(context.Context) atom.Engine { return &devJSONEngine{} }
	t.Cleanup(func() { testEngineFactory = nil })

	stdout, stderr, err := executeDev(t, "--once", "--json", "--path", job)
	require.NoError(t, err)
	require.NotContains(t, stderr, `"version"`)
	require.NotContains(t, stdout, "Run failed")
	require.NotContains(t, stdout, "caesium dev ")
	report := decodeDevReport(t, stdout)
	require.Equal(t, 1, report.Version)
	require.Equal(t, "dev", report.Command)
	require.True(t, report.OK)
	require.Equal(t, "dev", report.Scope.Kind)
	require.Equal(t, 1, report.Scope.Executed)
	require.Equal(t, 1, report.Discovered)
	require.Equal(t, 1, report.Selected)
	require.Len(t, report.Runs, 1)
	require.NotEmpty(t, report.Runs[0].RunID)
	require.NotEmpty(t, report.Runs[0].JobID)
	require.Equal(t, "succeeded", report.Runs[0].Status)
	require.NotEmpty(t, report.Runs[0].Tasks)
	require.Equal(t, "greet", report.Runs[0].Tasks[0].Name)
	require.NotEmpty(t, report.Runs[0].Tasks[0].TaskID)
}

func TestDevOnceJSONValidationFailureStaysJSON(t *testing.T) {
	dir := t.TempDir()
	job := filepath.Join(dir, "bad.job.yaml")
	require.NoError(t, os.WriteFile(job, []byte(`apiVersion: v1
kind: Job
metadata:
  alias: dev-bad
  schemaValidaton: fail
trigger:
  type: cron
  configuration:
    cron: "*/5 * * * *"
steps:
  - name: greet
    image: alpine:3.23
    command: ["echo", "hi"]
`), 0o644))

	stdout, stderr, err := executeDev(t, "--once", "--json", "--path", job)
	require.Error(t, err)
	require.NotContains(t, stderr, `"version"`)
	require.NotContains(t, stdout, "Run failed")
	report := decodeDevReport(t, stdout)
	require.Equal(t, 1, report.Version)
	require.False(t, report.OK)
	require.Empty(t, report.Runs)
	require.NotEmpty(t, report.Diagnostics)
	require.Equal(t, job, report.Diagnostics[0].File)
	require.NotZero(t, report.Diagnostics[0].Line)
	require.NotEmpty(t, report.Diagnostics[0].Path)
}

func executeDev(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	origPaths := append([]string(nil), devPaths...)
	origArgs := append([]string(nil), devParamArgs...)
	origParams := devParams
	origOnce := runOnce
	origJSON := devJSON
	origUsage := Cmd.SilenceUsage
	t.Cleanup(func() {
		devPaths = origPaths
		devParamArgs = origArgs
		devParams = origParams
		runOnce = origOnce
		devJSON = origJSON
		Cmd.SilenceUsage = origUsage
		for _, name := range []string{"path", "once", "json", "param"} {
			flag := Cmd.Flags().Lookup(name)
			if flag == nil {
				continue
			}
			_ = flag.Value.Set(flag.DefValue)
			flag.Changed = false
		}
		devPaths = origPaths
		devParamArgs = origArgs
		devParams = origParams
		runOnce = origOnce
		devJSON = origJSON
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

func decodeDevReport(t *testing.T, stdout string) offlinereport.Report {
	t.Helper()
	require.True(t, json.Valid([]byte(stdout)), "stdout was not JSON:\n%s", stdout)
	var report offlinereport.Report
	require.NoError(t, json.Unmarshal([]byte(stdout), &report))
	return report
}

type devJSONEngine struct{}

type devJSONAtom struct{ id string }

func (a *devJSONAtom) ID() string           { return a.id }
func (a *devJSONAtom) State() atom.State    { return atom.Stopped }
func (a *devJSONAtom) Result() atom.Result  { return atom.Success }
func (a *devJSONAtom) ExitCode() *int       { return nil }
func (a *devJSONAtom) CreatedAt() time.Time { return time.Now() }
func (a *devJSONAtom) StartedAt() time.Time { return time.Now() }
func (a *devJSONAtom) StoppedAt() time.Time { return time.Now() }
func (a *devJSONAtom) Engine() atom.Engine  { return nil }

func (e *devJSONEngine) Create(req *atom.EngineCreateRequest) (atom.Atom, error) {
	return &devJSONAtom{id: "atom-" + req.Name}, nil
}
func (e *devJSONEngine) Get(req *atom.EngineGetRequest) (atom.Atom, error) {
	return &devJSONAtom{id: req.ID}, nil
}
func (e *devJSONEngine) List(*atom.EngineListRequest) ([]atom.Atom, error) { return nil, nil }
func (e *devJSONEngine) Wait(req *atom.EngineWaitRequest) (atom.Atom, error) {
	return &devJSONAtom{id: req.ID}, nil
}
func (e *devJSONEngine) Stop(*atom.EngineStopRequest) error { return nil }
func (e *devJSONEngine) Logs(*atom.EngineLogsRequest) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (e *devJSONEngine) Stats(*atom.EngineStatsRequest) (atom.ResourceStats, error) {
	return atom.ResourceStats{}, atom.ErrStatsUnavailable
}
