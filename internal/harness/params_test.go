package harness

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/stretchr/testify/require"
)

func TestCollectScenariosLoadsParamsAndRejectsUnknownParamField(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "job.job.yaml"), []byte(paramJobManifest()), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ok.scenario.yaml"), []byte(`
apiVersion: v1
kind: Harness
scenarios:
  - name: override
    path: ./job.job.yaml
    params:
      date: "2026-09-15"
      note: "a,b"
    expect:
      runStatus: succeeded
`), 0o644))

	scenarios, err := CollectScenarios([]string{filepath.Join(dir, "ok.scenario.yaml")})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"date": "2026-09-15", "note": "a,b"}, scenarios[0].Scenario.Params)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "typo.scenario.yaml"), []byte(`
apiVersion: v1
kind: Harness
scenarios:
  - name: typo
    path: ./job.job.yaml
    param:
      date: "2026-09-15"
    expect:
      runStatus: succeeded
`), 0o644))
	_, err = CollectScenarios([]string{filepath.Join(dir, "typo.scenario.yaml")})
	require.Error(t, err)
	require.Contains(t, err.Error(), `YAML path scenarios[0].param`)
	require.Contains(t, err.Error(), `unknown field "param"`)
}

func TestExecuteScenarioParamsOverrideTriggerDefaults(t *testing.T) {
	t.Run("defaults with no scenario params", func(t *testing.T) {
		env := executeParamScenario(t, "")
		require.Equal(t, "2026-09-14", env["DAY"])
		require.Equal(t, "2026-09-14", env["CAESIUM_PARAM_DATE"])
	})

	t.Run("scenario params override defaults", func(t *testing.T) {
		env := executeParamScenario(t, `
    params:
      date: "2026-09-15"
`)
		require.Equal(t, "2026-09-15", env["DAY"])
		require.Equal(t, "2026-09-15", env["CAESIUM_PARAM_DATE"])
		require.NotContains(t, env["DAY"], "${CAESIUM_PARAM_")
	})
}

func executeParamScenario(t *testing.T, paramsYAML string) map[string]string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "job.job.yaml"), []byte(paramJobManifest()), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "case.scenario.yaml"), []byte(`
apiVersion: v1
kind: Harness
scenarios:
  - name: dated
    path: ./job.job.yaml
`+paramsYAML+`
    expect:
      runStatus: succeeded
      tasks:
        - name: extract
          status: succeeded
`), 0o644))

	scenarios, err := CollectScenarios([]string{filepath.Join(dir, "case.scenario.yaml")})
	require.NoError(t, err)
	require.Len(t, scenarios, 1)

	engine := &scenarioCaptureEngine{}
	testEngineFactory = func(context.Context) atom.Engine { return engine }
	t.Cleanup(func() { testEngineFactory = nil })

	result, err := Execute(context.Background(), scenarios[0])
	require.NoError(t, err)
	require.Empty(t, result.Failures)
	require.True(t, result.Passed())
	require.NotNil(t, engine.env)
	return engine.env
}

func paramJobManifest() string {
	return `apiVersion: v1
kind: Job
metadata:
  alias: harness-dated
trigger:
  type: cron
  configuration:
    cron: "0 2 * * *"
  defaultParams:
    date: "2026-09-14"
steps:
  - name: extract
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    env:
      DAY: "${CAESIUM_PARAM_DATE}"
`
}

type scenarioCaptureEngine struct {
	mu  sync.Mutex
	env map[string]string
}

type scenarioCaptureAtom struct{ id string }

func (a *scenarioCaptureAtom) ID() string           { return a.id }
func (a *scenarioCaptureAtom) State() atom.State    { return atom.Stopped }
func (a *scenarioCaptureAtom) Result() atom.Result  { return atom.Success }
func (a *scenarioCaptureAtom) ExitCode() *int       { return nil }
func (a *scenarioCaptureAtom) CreatedAt() time.Time { return time.Now() }
func (a *scenarioCaptureAtom) StartedAt() time.Time { return time.Now() }
func (a *scenarioCaptureAtom) StoppedAt() time.Time { return time.Now() }
func (a *scenarioCaptureAtom) Engine() atom.Engine  { return nil }

func (e *scenarioCaptureEngine) Create(req *atom.EngineCreateRequest) (atom.Atom, error) {
	e.mu.Lock()
	e.env = make(map[string]string, len(req.Spec.Env))
	for key, value := range req.Spec.Env {
		e.env[key] = value
	}
	e.mu.Unlock()
	return &scenarioCaptureAtom{id: "atom-" + req.Name}, nil
}

func (e *scenarioCaptureEngine) Get(req *atom.EngineGetRequest) (atom.Atom, error) {
	return &scenarioCaptureAtom{id: req.ID}, nil
}

func (e *scenarioCaptureEngine) List(*atom.EngineListRequest) ([]atom.Atom, error) {
	return nil, nil
}

func (e *scenarioCaptureEngine) Wait(req *atom.EngineWaitRequest) (atom.Atom, error) {
	return &scenarioCaptureAtom{id: req.ID}, nil
}

func (e *scenarioCaptureEngine) Stop(*atom.EngineStopRequest) error { return nil }

func (e *scenarioCaptureEngine) Logs(*atom.EngineLogsRequest) (io.ReadCloser, error) {
	return io.NopCloser(io.MultiReader()), nil
}

func (e *scenarioCaptureEngine) Stats(*atom.EngineStatsRequest) (atom.ResourceStats, error) {
	return atom.ResourceStats{}, atom.ErrStatsUnavailable
}
