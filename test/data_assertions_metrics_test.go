//go:build integration

package test

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
)

// runCLIWithDataAssertions runs the CLI with CAESIUM_DATA_ASSERTIONS_ENABLED=true
// added to its environment.
//
// `caesium job apply` validates the manifest CLIENT-side before it posts, and
// pkg/jobdef gates the assertions surface on that variable exactly as it gates
// `trigger.type: freshness` on CAESIUM_FRESHNESS_ENABLED — so an operator
// applying a manifest with assertions sets the flag in their own shell too.
// H-1 turns the flag on for every lane's SERVER; the test-runner container is a
// separate process, so the scenario supplies it here rather than skipping.
func (s *IntegrationTestSuite) runCLIWithDataAssertions(args ...string) {
	s.T().Helper()
	cmd := exec.CommandContext(s.T().Context(), s.cliPath, args...)
	cmd.Dir = s.projectRoot
	cmd.Env = append(os.Environ(), "CAESIUM_DATA_ASSERTIONS_ENABLED=true")
	output, err := cmd.CombinedOutput()
	require.NoError(s.T(), err, "cli %v failed: %s", args, string(output))
}

// TestDataAssertionsMetricsPersisted drives the Phase 0 observability substrate
// end to end on a live server: a real container emits ##caesium::metrics, and
// the samples land as DatasetMetric rows attributed to the task run that
// emitted them, against the dataset the step declares.
//
// It reads the rows through the shared catalog handle rather than
// GET /v1/datasets/:ns/:name/metrics, because that route belongs to Stream D
// (W4) and does not exist yet; D1 replaces this read with the route-based
// check. Recorded as a deliberate, temporary deviation from acceptance
// criterion 1.
func (s *IntegrationTestSuite) TestDataAssertionsMetricsPersisted() {
	s.requireDataAssertionsLane()

	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("integration-metrics-%d", suffix)
	dataset := fmt.Sprintf("integration.metrics.%d", suffix)
	watermark := "2026-07-03T01:12:00Z"

	step, err := metricsProducerStepForDataset("load", dataset, map[string]any{
		"rowCount":       10400312,
		"dedup_ratio":    0.01,
		"max_event_time": watermark,
	})
	s.Require().NoError(err)

	dir := s.writeJobManifest(metricsProducerManifest(alias, dataset, step))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Require().Equal("succeeded", run.Status, "a metrics emitter must succeed: Phase 0 observes, it does not enforce")

	conn := s.openIntegrationCatalogGorm()

	var rows []models.DatasetMetric
	s.Require().Eventually(func() bool {
		rows = nil
		if err := conn.Where("name = ?", dataset).Order("metric ASC").Find(&rows).Error; err != nil {
			return false
		}
		return len(rows) == 3
	}, 60*time.Second, 500*time.Millisecond, "expected 3 dataset_metrics rows for %s", dataset)

	byMetric := make(map[string]models.DatasetMetric, len(rows))
	for _, row := range rows {
		byMetric[row.Metric] = row
		s.Equal(dataset, row.Name)
		s.Equal("", row.Namespace, "namespace is reserved in v1")
		s.NotEqual("", row.TaskRunID.String(), "every sample attributes to the task run that emitted it")
	}

	s.Require().Contains(byMetric, "rowCount")
	s.InDelta(10400312, byMetric["rowCount"].Value, 0.5)

	s.Require().Contains(byMetric, "dedup_ratio", "an undeclared metric is recorded too: free baseline history")
	s.InDelta(0.01, byMetric["dedup_ratio"].Value, 0.0001)

	s.Require().Contains(byMetric, "max_event_time")
	wantWatermark, err := time.Parse(time.RFC3339, watermark)
	s.Require().NoError(err)
	s.InDelta(float64(wantWatermark.Unix()), byMetric["max_event_time"].Value, 0.5,
		"an RFC3339 metric is stored as epoch seconds")

	// The samples must hang off a task run of THIS run, not an arbitrary row.
	var taskRunIDs []string
	s.Require().NoError(conn.Model(&models.TaskRun{}).
		Where("job_run_id = ?", runID).
		Pluck("id", &taskRunIDs).Error)
	s.Require().NotEmpty(taskRunIDs)
	s.Contains(taskRunIDs, byMetric["rowCount"].TaskRunID.String())
}

// TestDataAssertionsSchemaPersistsOnTheRegistry proves the declared assertion
// spec survives the CLI → server → registry path and lands on the SAME
// dataset_declarations row as the freshness SLO — one registry, no private copy.
func (s *IntegrationTestSuite) TestDataAssertionsSchemaPersistsOnTheRegistry() {
	s.requireDataAssertionsLane()

	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("integration-assertions-%d", suffix)
	dataset := fmt.Sprintf("integration.assertions.%d", suffix)

	step, err := metricsProducerStepForDataset("load", dataset, map[string]any{
		"rowCount":    10400312,
		"dedup_ratio": 0.01,
	})
	s.Require().NoError(err)

	dir := s.writeJobManifest(assertionsProducerManifest(alias, dataset, step))
	defer os.RemoveAll(dir)

	s.runCLIWithDataAssertions("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	conn := s.openIntegrationCatalogGorm()

	var decl models.DatasetDeclaration
	s.Require().Eventually(func() bool {
		return conn.Where("name = ? AND direction = ?", dataset, models.DatasetDirectionProduces).
			First(&decl).Error == nil
	}, 30*time.Second, 250*time.Millisecond, "expected a produces declaration for %s", dataset)

	s.Equal("hold", decl.OnViolation)
	s.Equal("manual", decl.Release)
	s.Contains(decl.AssertionsJSON, `"rowCount"`)
	s.Contains(decl.AssertionsJSON, `"50%"`)
	s.Contains(decl.AssertionsJSON, `"dedup_ratio"`)
	// The SLO column the freshness plan owns must still be on the same row.
	s.Equal("6h", decl.Freshness)
}

// metricsProducerManifest wraps H-1's metrics-emitting fixture step in a job
// that declares the dataset the metrics belong to.
func metricsProducerManifest(alias, dataset, step string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
%s    datasets:
      produces:
        - name: %s
          freshness: 6h
`, alias, step, dataset)
}

// assertionsProducerManifest is the design's worked example expressed in the
// shipped YAML nesting: assertions + onViolation + release on a produced
// dataset under steps[].datasets.produces.
func assertionsProducerManifest(alias, dataset, step string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
%s    datasets:
      produces:
        - name: %s
          freshness: 6h
          assertions:
            rowCount: {min: 1000, deltaFromBaseline: 50%%}
            custom:
              - {metric: dedup_ratio, max: 0.05}
          onViolation: hold
          release: manual
`, alias, step, dataset)
}
