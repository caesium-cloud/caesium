//go:build integration

package test

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
)

// systemFeatures is the subset of GET /v1/system/features this stream cares
// about. It is read through the real endpoint, not the env, so the assertion
// proves the flag actually reaches the API surface.
type systemFeatures struct {
	DataAssertionsEnabled bool `json:"data_assertions_enabled"`
}

func (s *IntegrationTestSuite) dataAssertionsFeature() systemFeatures {
	s.T().Helper()
	var features systemFeatures
	s.getJSON("/v1/system/features", &features)
	return features
}

// requireDataAssertionsEnabled skips loudly rather than failing when the lane's
// server does not carry CAESIUM_DATA_ASSERTIONS_ENABLED=true. A lane without
// the flag is honest about not covering the feature; it is not red.
func (s *IntegrationTestSuite) requireDataAssertionsEnabled() {
	s.T().Helper()
	if !s.dataAssertionsFeature().DataAssertionsEnabled {
		s.T().Skip("server reports data_assertions_enabled=false; set CAESIUM_DATA_ASSERTIONS_ENABLED=true on this lane's server (justfile integration-up) to cover the data circuit breaker")
	}
}

// TestDataAssertionsMetricsPersisted drives the Phase 0 observability substrate
// end to end on a live server: a step emits ##caesium::metrics from a real
// container, and the samples land as DatasetMetric rows attributed to the
// task run that emitted them, against the dataset the step declares.
//
// It reads the rows through the shared catalog handle rather than
// GET /v1/datasets/:ns/:name/metrics, because that route is Stream D's (W4) and
// does not exist yet; D1 replaces this read with the route-based check.
func (s *IntegrationTestSuite) TestDataAssertionsMetricsPersisted() {
	s.requireDataAssertionsEnabled()

	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("integration-metrics-%d", suffix)
	dataset := fmt.Sprintf("integration.metrics.%d", suffix)
	watermark := "2026-07-03T01:12:00Z"

	dir := s.writeJobManifest(metricsProducerManifest(alias, dataset, watermark))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Require().Equal("succeeded", run.Status, "the metrics emitter must succeed; metrics are observability, not enforcement")

	conn := s.openIntegrationCatalogGorm()

	var rows []models.DatasetMetric
	s.Require().Eventually(func() bool {
		rows = nil
		if err := conn.Where("name = ?", dataset).Order("metric ASC").Find(&rows).Error; err != nil {
			return false
		}
		return len(rows) == 3
	}, 60*time.Second, 500*time.Millisecond, "expected 3 dataset_metrics rows for %s, saw %d", dataset, len(rows))

	byMetric := make(map[string]models.DatasetMetric, len(rows))
	for _, row := range rows {
		byMetric[row.Metric] = row
		s.Equal(dataset, row.Name)
		s.Equal("", row.Namespace, "namespace is reserved in v1")
		s.NotEmpty(row.TaskRunID, "every sample attributes to the task run that emitted it")
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
// dataset_declarations row as the freshness SLO (one registry, no private copy).
//
// `caesium job apply` validates client-side, so the manifest is only accepted
// when the RUNNER container also carries CAESIUM_DATA_ASSERTIONS_ENABLED=true —
// that is the harness half (W1-β / H-1). Until it lands the scenario skips with
// a message naming the gap instead of failing.
func (s *IntegrationTestSuite) TestDataAssertionsSchemaPersistsOnTheRegistry() {
	s.requireDataAssertionsEnabled()
	if enabled, _ := strconv.ParseBool(strings.TrimSpace(os.Getenv("CAESIUM_DATA_ASSERTIONS_ENABLED"))); !enabled {
		s.T().Skip("CAESIUM_DATA_ASSERTIONS_ENABLED is not set in the test-runner container, so `caesium job apply` would refuse the assertions block client-side; H-1 adds it to the runner env")
	}

	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("integration-assertions-%d", suffix)
	dataset := fmt.Sprintf("integration.assertions.%d", suffix)

	dir := s.writeJobManifest(assertionsProducerManifest(alias, dataset))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
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
	// The SLO columns the freshness plan owns must still be on the same row.
	s.Equal("6h", decl.Freshness)
}

// metricsProducerManifest declares one produced dataset and emits three metrics
// for it: a count, an undeclared ratio, and an RFC3339 watermark.
func metricsProducerManifest(alias, dataset, watermark string) string {
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
  - name: load
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::metrics {\"dataset\":\"%s\",\"rowCount\":10400312,\"dedup_ratio\":0.01,\"max_event_time\":\"%s\"}'"]
    datasets:
      produces:
        - name: %s
          freshness: 6h
`, alias, dataset, watermark, dataset)
}

// assertionsProducerManifest is the design's worked example in the shipped YAML
// nesting: assertions + onViolation + release on a produced dataset.
func assertionsProducerManifest(alias, dataset string) string {
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
  - name: load
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::metrics {\"dataset\":\"%s\",\"rowCount\":10400312,\"dedup_ratio\":0.01}'"]
    datasets:
      produces:
        - name: %s
          freshness: 6h
          assertions:
            rowCount: {min: 1000, deltaFromBaseline: 50%%}
            custom:
              - {metric: dedup_ratio, max: 0.05}
          onViolation: hold
          release: manual
`, alias, dataset, dataset)
}
