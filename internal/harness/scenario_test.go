package harness

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCollectScenariosLoadsScenarioFiles(t *testing.T) {
	dir := t.TempDir()

	jobPath := filepath.Join(dir, "job.job.yaml")
	require.NoError(t, os.WriteFile(jobPath, []byte(`
apiVersion: v1
kind: Job
metadata:
  alias: harness-job
trigger:
  type: cron
  configuration:
    cron: "*/5 * * * *"
steps:
  - name: hello
    image: alpine:3.23
    command: ["echo", "hello"]
`), 0o644))

	scenarioPath := filepath.Join(dir, "smoke.scenario.yaml")
	require.NoError(t, os.WriteFile(scenarioPath, []byte(`
apiVersion: v1
kind: Harness
scenarios:
  - name: smoke
    path: ./job.job.yaml
    expect:
      runStatus: succeeded
      tasks:
        - name: hello
          status: succeeded
`), 0o644))

	scenarios, err := CollectScenarios([]string{dir})
	require.NoError(t, err)
	require.Len(t, scenarios, 1)
	require.Equal(t, "smoke", scenarios[0].Scenario.Name)

	def, err := scenarios[0].Definition()
	require.NoError(t, err)
	require.Equal(t, "harness-job", def.Metadata.Alias)
}

func TestCollectScenariosRejectsDuplicateTaskExpectations(t *testing.T) {
	dir := t.TempDir()
	scenarioPath := filepath.Join(dir, "broken.scenario.yaml")
	require.NoError(t, os.WriteFile(scenarioPath, []byte(`
apiVersion: v1
kind: Harness
scenarios:
  - name: broken
    path: ./job.job.yaml
    expect:
      tasks:
        - name: hello
        - name: hello
`), 0o644))

	_, err := CollectScenarios([]string{scenarioPath})
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate expected task")
}

func TestCollectScenariosRejectsMetricWithoutValueOrDelta(t *testing.T) {
	dir := t.TempDir()
	scenarioPath := filepath.Join(dir, "broken.scenario.yaml")
	require.NoError(t, os.WriteFile(scenarioPath, []byte(`
apiVersion: v1
kind: Harness
scenarios:
  - name: broken
    path: ./job.job.yaml
    expect:
      metrics:
        - name: caesium_job_runs_total
          labels:
            job_id: $job_id
            status: succeeded
`), 0o644))

	_, err := CollectScenarios([]string{scenarioPath})
	require.Error(t, err)
	require.Contains(t, err.Error(), "must set value or delta")
}
