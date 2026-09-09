//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	datasetsvc "github.com/caesium-cloud/caesium/api/rest/service/dataset"
	"github.com/caesium-cloud/caesium/internal/models"
)

// fetchDatasetOperatorJSON always drives the protected HTTP surface. These
// scenarios never seed catalog rows or call the evaluator directly.
func (s *IntegrationTestSuite) fetchDatasetOperatorJSON(path string, result any) {
	s.T().Helper()
	resp, err := s.doJSONRequest(http.MethodGet, s.caesiumURL+path, nil)
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Require().Equal(http.StatusOK, resp.StatusCode, "GET %s", path)
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(result), "GET %s", path)
}

func datasetOperatorPath(name string) string {
	return "/v1/datasets/_/" + url.PathEscape(name)
}

func (s *IntegrationTestSuite) fetchDatasetOperatorHolds(name, status string) datasetsvc.HoldsResult {
	s.T().Helper()
	var result datasetsvc.HoldsResult
	s.fetchDatasetOperatorJSON("/v1/datasets/holds?"+url.Values{"status": {status}, "namespace": {""}, "name": {name}}.Encode(), &result)
	return result
}

// TestDataAssertionsDatasetOperatorReads proves both new GET routes, the
// enriched existing list/detail, and the holds/metrics CLI against real samples.
// A slash is part of the declared Name; it must survive HTTP path escaping.
func (s *IntegrationTestSuite) TestDataAssertionsDatasetOperatorReads() {
	s.requireDataAssertionsLane()
	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("integration-operator-reads-%d", suffix)
	dataset := fmt.Sprintf("warehouse/operator-%d", suffix)
	producer := s.applyHoldProducer(alias, dataset, "auto", 2000)
	clean := s.runProducer(producer)
	s.Require().NotEmpty(clean.Tasks)

	// Poll the public read because owner-memory runs mirror catalog task state
	// asynchronously; the baseline must eventually see the real succeeded run.
	var metric datasetsvc.MetricsResult
	s.Require().Eventually(func() bool {
		s.fetchDatasetOperatorJSON(datasetOperatorPath(dataset)+"/metrics?metric=rowCount", &metric)
		return len(metric.Series) == 1 && metric.Baseline != nil && metric.Baseline.Samples == 1
	}, 30*time.Second, 250*time.Millisecond)
	s.Equal("", metric.Namespace)
	s.Equal(dataset, metric.Name)
	s.Equal("rowCount", metric.Metric)
	s.Equal(2000.0, metric.Series[0].Value)
	s.False(metric.Series[0].Violated)
	s.True(metric.Series[0].InBaseline)
	s.Equal(2000.0, metric.Baseline.Median)
	s.Equal(2000.0, metric.Baseline.P10)
	s.Equal(2000.0, metric.Baseline.P90)
	s.Equal([]float64{2000}, metric.Baseline.Values)
	s.Equal(metric.Baseline.Samples < metric.MinSamples, metric.Seeding)
	s.NotEmpty(metric.Series[0].TaskRunID)

	producer = s.applyHoldProducer(alias, dataset, "auto", 12)
	breach := s.runProducer(producer)
	holds := s.fetchDatasetOperatorHolds(dataset, "active")
	s.Require().EqualValues(1, holds.Total)
	s.Require().Len(holds.Holds, 1, "the literal /datasets/holds route must resolve ahead of dataset parameters")
	hold := holds.Holds[0]
	s.Equal(dataset, hold.Name)
	s.Equal("", hold.Namespace)
	s.Require().NotNil(hold.HeldByRunID)
	s.Equal(breach.ID, hold.HeldByRunID.String())
	s.NotEmpty(hold.Violations)

	var detail datasetsvc.Detail
	s.fetchDatasetOperatorJSON(datasetOperatorPath(dataset), &detail)
	s.Require().NotNil(detail.Hold)
	s.Equal(hold.ID, detail.Hold.ID)
	s.Equal("active", detail.HoldStatus)
	var metadata datasetsvc.Detail
	s.fetchDatasetOperatorJSON(datasetOperatorPath(dataset)+"?include_hold=false", &metadata)
	s.Nil(metadata.Hold)
	s.Empty(metadata.HoldStatus)
	s.Equal(detail.State, metadata.State)
	var listed datasetsvc.ListResult
	s.fetchDatasetOperatorJSON("/v1/datasets?limit=200", &listed)
	found := false
	for _, row := range listed.Datasets {
		if row.Namespace == "" && row.Name == dataset {
			found = true
			s.Require().NotNil(row.Hold)
			s.Equal(hold.ID, row.Hold.ID)
			s.Equal("active", row.HoldStatus)
			s.Equal(detail.State.Status, row.Status, "hold enrichment preserves the existing freshness projection")
		}
	}
	s.True(found, "the dataset must remain in the existing registry feed")
	// Decode the observed payload without a typed projection so unexpected
	// heavyweight JSON cannot disappear during unmarshalling and hide a regression.
	var rawList struct {
		Datasets []map[string]json.RawMessage `json:"datasets"`
	}
	s.fetchDatasetOperatorJSON("/v1/datasets?limit=200", &rawList)
	for _, row := range rawList.Datasets {
		if string(row["name"]) != strconv.Quote(dataset) {
			continue
		}
		var summary map[string]json.RawMessage
		s.Require().NoError(json.Unmarshal(row["hold"], &summary))
		s.Contains(summary, "id")
		s.NotContains(summary, "violations")
		s.NotContains(summary, "impact")
	}

	s.fetchDatasetOperatorJSON(datasetOperatorPath(dataset)+"/metrics?metric=rowCount", &metric)
	s.Require().Len(metric.Series, 2)
	s.Equal(12.0, metric.Series[1].Value)
	s.True(metric.Series[1].Violated)
	s.False(metric.Series[1].InBaseline)
	s.Equal(1, metric.Baseline.Samples, "a held violating sample must remain outside the clean baseline")
	s.Equal([]float64{2000}, metric.Baseline.Values)
	for offset := 0; offset < 2; offset++ {
		stdout, err := s.runCLIStdout("dataset", "metrics", dataset, "--metric", "rowCount", "--limit", "1", "--offset", strconv.Itoa(offset), "--json", "--server", s.caesiumURL)
		s.Require().NoError(err)
		var page datasetsvc.MetricsResult
		s.Require().NoError(json.Unmarshal([]byte(stdout), &page))
		s.EqualValues(2, page.Total)
		s.Equal(1, page.Limit)
		s.Equal(offset, page.Offset)
		s.Require().Len(page.Series, 1)
		s.Equal(metric.Series[1-offset].ID, page.Series[0].ID)
		s.Equal(offset == 1, page.Series[0].InBaseline)
		s.Equal([]float64{2000}, page.Baseline.Values, "paging raw history must not page the clean baseline")
		stdout, err = s.runCLIStdout("dataset", "list", "--limit", "1", "--offset", strconv.Itoa(offset), "--json", "--server", s.caesiumURL)
		s.Require().NoError(err)
		var states datasetsvc.ListResult
		s.Require().NoError(json.Unmarshal([]byte(stdout), &states))
		s.Equal(1, states.Limit)
		s.Equal(offset, states.Offset)
		s.GreaterOrEqual(states.Total, int64(1))
	}

	for _, args := range [][]string{
		{"dataset", "holds", "--json"},
		{"dataset", "holds", "--all-namespaces", "--json"},
		{"dataset", "metrics", dataset, "--metric", "rowCount", "--json"},
		{"dataset", "status", dataset, "--json"},
		{"dataset", "list", "--json"},
	} {
		stdout, err := s.runCLIStdout(append(args, "--server", s.caesiumURL)...)
		s.Require().NoError(err)
		s.True(json.Valid([]byte(stdout)), "machine output must be one clean JSON value: %s", stdout)
		if args[1] == "metrics" {
			var result datasetsvc.MetricsResult
			s.Require().NoError(json.Unmarshal([]byte(stdout), &result))
			s.Require().Len(result.Series, 2)
			s.Equal(dataset, result.Name)
			s.Equal(1, result.Baseline.Samples)
		}
		if args[1] == "status" {
			var result datasetsvc.Detail
			s.Require().NoError(json.Unmarshal([]byte(stdout), &result))
			s.Require().NotNil(result.Hold)
			s.Equal(hold.ID, result.Hold.ID)
			s.Equal(dataset, result.State.Name)
		}
		if args[1] == "holds" {
			var result datasetsvc.HoldsResult
			s.Require().NoError(json.Unmarshal([]byte(stdout), &result))
			ids := make([]string, 0, len(result.Holds))
			for _, h := range result.Holds {
				ids = append(ids, h.ID.String())
			}
			s.Contains(ids, hold.ID.String())
		}
	}
	// Exercise explicit page selection through the real CLI and feed. Only
	// this scenario's one new hold is needed; offset 1 may be an empty page
	// when the server has no older holds, but must still echo the requested
	// position and the unpaginated total.
	for offset := 0; offset < 2; offset++ {
		stdout, err := s.runCLIStdout("dataset", "holds", "--namespace", "_", "--limit", "1", "--offset", strconv.Itoa(offset), "--json", "--server", s.caesiumURL)
		s.Require().NoError(err)
		var page datasetsvc.HoldsResult
		s.Require().NoError(json.Unmarshal([]byte(stdout), &page), "page JSON must be clean stdout")
		s.Equal(1, page.Limit)
		s.Equal(offset, page.Offset)
		s.Require().GreaterOrEqual(page.Total, int64(1))
		if offset == 0 {
			s.Require().Len(page.Holds, 1)
			s.Equal(hold.ID, page.Holds[0].ID, "the just-opened hold is the newest page")
		} else if page.Total > int64(offset) {
			s.Require().Len(page.Holds, 1)
			s.NotEqual(hold.ID, page.Holds[0].ID, "offset must advance past the first page")
		} else {
			s.Empty(page.Holds)
		}
	}
	for _, path := range []string{"/v1/datasets/holds?status=bogus", datasetOperatorPath(dataset) + "/metrics", datasetOperatorPath(dataset) + "/metrics?metric=dataset", datasetOperatorPath(dataset) + "/metrics?metric=rowCount&limit=bad", datasetOperatorPath(dataset) + "?include_hold=bad"} {
		resp, err := s.doJSONRequest(http.MethodGet, s.caesiumURL+path, nil)
		s.Require().NoError(err)
		resp.Body.Close()
		s.Equal(http.StatusBadRequest, resp.StatusCode, path)
	}
	var unknownMetric datasetsvc.MetricsResult
	s.fetchDatasetOperatorJSON(datasetOperatorPath(dataset)+"/metrics?metric=never_emitted", &unknownMetric)
	s.Empty(unknownMetric.Series)
	s.Equal(0, unknownMetric.Baseline.Samples)
	unknown, err := s.doJSONRequest(http.MethodGet, s.caesiumURL+datasetOperatorPath(dataset+"-missing")+"/metrics?metric=rowCount", nil)
	s.Require().NoError(err)
	unknown.Body.Close()
	s.Equal(http.StatusNotFound, unknown.StatusCode)

	// The read feed observes clean-run release too; no DB shortcut is needed.
	producer = s.applyHoldProducer(alias, dataset, "auto", 3000)
	s.runProducer(producer)
	s.Empty(s.fetchDatasetOperatorHolds(dataset, "active").Holds)
	released := s.fetchDatasetOperatorHolds(dataset, "released")
	s.Require().Len(released.Holds, 1)
	s.Equal(models.DatasetHoldReleaseCleanRun, released.Holds[0].ReleaseReason)
	var after datasetsvc.Detail
	s.fetchDatasetOperatorJSON(datasetOperatorPath(dataset), &after)
	s.Empty(after.HoldStatus)
	s.Nil(after.Hold)
}

// An unviolated metric from a failed task remains visible but is not clean
// baseline evidence. Drive the evaluator and task completion before reading it.
func (s *IntegrationTestSuite) TestDataAssertionsDatasetMetricBaselineMembership() {
	s.requireDataAssertionsLane()
	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("integration-operator-membership-%d", suffix)
	dataset := fmt.Sprintf("warehouse/membership-%d", suffix)
	step, err := metricsProducerStepForDataset("load", dataset, map[string]any{"rowCount": 12, "dedup_ratio": 0.01})
	s.Require().NoError(err)
	producer := s.applyAssertionsJob(alias, assertionsEvaluatorManifest(alias, dataset, step,
		"            rowCount: {min: 1000}", "fail"))
	runID := s.triggerRun(producer.ID)
	run := s.awaitRun(producer.ID, runID, runTimeout)
	s.Require().Equal("failed", run.Status)
	var result datasetsvc.MetricsResult
	s.fetchDatasetOperatorJSON(datasetOperatorPath(dataset)+"/metrics?metric=dedup_ratio", &result)
	s.Require().Len(result.Series, 1)
	s.False(result.Series[0].Violated)
	s.False(result.Series[0].InBaseline, "failed-run samples must not appear in the clean baseline")
	s.Equal(0, result.Baseline.Samples)
}

// TestHoldDatasetCLIReleaseReopensTheGate runs visibly on the auth lane. The
// operator resolves a live hold by exact dataset identity through the GET feed,
// releases it via the CLI binary, and the next consumer actually runs.
func (s *IntegrationTestSuite) TestHoldDatasetCLIReleaseReopensTheGate() {
	s.requireAuthLane()
	s.requireDataAssertionsLane()
	suffix := time.Now().UnixNano()
	dataset := fmt.Sprintf("warehouse/operator-ack-%d", suffix)
	producer := s.applyHoldProducer(fmt.Sprintf("integration-operator-ack-producer-%d", suffix), dataset, "manual", 12)
	s.runProducer(producer)
	consumerAlias := fmt.Sprintf("integration-operator-ack-consumer-%d", suffix)
	consumer := s.applyAssertionsJob(consumerAlias, holdConsumerManifest(consumerAlias, dataset, ""))
	held := s.triggerHeldRun(consumer.ID)
	s.Require().Equal("skipped", held.Status)
	s.Equal("dataset_hold:/"+dataset, held.SkipReason)
	holds := s.fetchDatasetOperatorHolds(dataset, "active")
	s.Require().Len(holds.Holds, 1)
	hold := holds.Holds[0]
	s.Equal(s.holdIDFromSkippedRun(held), hold.ID.String())

	missingReasonOut, missingReasonErr := s.runCLIStdout("dataset", "release", dataset, "--json", "--server", s.caesiumURL)
	s.Require().ErrorContains(missingReasonErr, "--reason is required")
	s.Empty(missingReasonOut)

	// With no client credential, resolution is refused by auth middleware.
	apiKey := os.Getenv("CAESIUM_API_KEY")
	s.T().Setenv("CAESIUM_API_KEY", "")
	noKeyOut, noKeyErr := s.runCLIStdout("dataset", "release", dataset, "--reason", "anonymous ack", "--json", "--server", s.caesiumURL)
	s.T().Setenv("CAESIUM_API_KEY", apiKey)
	s.Require().ErrorContains(noKeyErr, "401")
	s.Empty(noKeyOut)

	viewer := s.createAPIKeyCLI("--role", "viewer", "--description", "dataset operator read coverage")
	for _, path := range []string{"/v1/datasets/holds?status=active", datasetOperatorPath(dataset) + "/metrics?metric=rowCount"} {
		status, body := s.requestWithKey(http.MethodGet, path, viewer.Plaintext, nil)
		s.Equal(http.StatusOK, status, "viewer GET %s: %s", path, body)
		s.True(json.Valid([]byte(body)))
	}
	stdout, err := s.runCLIStdout("dataset", "release", dataset, "--reason", "viewer must not ack", "--api-key", viewer.Plaintext, "--json", "--server", s.caesiumURL)
	s.Require().ErrorContains(err, "403")
	s.Empty(stdout)
	// Explicit namespace never falls back to the identically spelled bare name.
	stdout, err = s.runCLIStdout("dataset", "release", dataset, "--namespace", "another", "--reason", "wrong identity", "--json", "--server", s.caesiumURL)
	s.Require().ErrorContains(err, "no active hold exists for dataset another/"+dataset)
	s.Empty(stdout)
	s.Require().Len(s.fetchDatasetOperatorHolds(dataset, "active").Holds, 1)

	stdout, err = s.runCLIStdout("dataset", "release", dataset, "--namespace", "_", "--reason", "source verified by operator", "--tolerate", "min=24h", "--json", "--server", s.caesiumURL)
	s.Require().NoError(err)
	var result datasetsvc.ReleaseHoldResult
	s.Require().NoError(json.Unmarshal([]byte(stdout), &result), "release JSON must be clean stdout")
	s.Require().NotNil(result.Hold)
	s.Equal(hold.ID, result.Hold.ID)
	s.Equal(dataset, result.Hold.Name)
	s.Equal("released", result.Hold.Status)
	s.Equal("manual_ack", result.Hold.ReleaseReason)
	s.NotEmpty(result.Hold.ReleasedBy)
	s.JSONEq(`{"min":"24h"}`, string(result.Hold.Tolerances))
	s.Empty(s.fetchDatasetOperatorHolds(dataset, "active").Holds)
	s.Require().Len(s.fetchDatasetOperatorHolds(dataset, "released").Holds, 1)
	runID := s.triggerRun(consumer.ID)
	reopened := s.awaitRun(consumer.ID, runID, runTimeout)
	s.Equal("succeeded", reopened.Status)
	s.Empty(reopened.SkipReason)

	stdout, err = s.runCLIStdout("dataset", "release", dataset, "--reason", "already clear", "--json", "--server", s.caesiumURL)
	s.Require().ErrorContains(err, "no active hold exists for dataset _/"+dataset)
	s.Empty(stdout)
}

func (s *IntegrationTestSuite) TestDataAssertionsDatasetCLIReleaseRefusedWithoutAuth() {
	s.skipOnAuthLane("manual release requires an authenticated deployment")
	s.requireDataAssertionsLane()
	suffix := time.Now().UnixNano()
	dataset := fmt.Sprintf("warehouse/operator-no-auth-%d", suffix)
	producer := s.applyHoldProducer(fmt.Sprintf("integration-operator-no-auth-%d", suffix), dataset, "manual", 12)
	s.runProducer(producer)
	s.Require().Len(s.fetchDatasetOperatorHolds(dataset, "active").Holds, 1)
	stdout, err := s.runCLIStdout("dataset", "release", dataset, "--reason", "anonymous release", "--json", "--server", s.caesiumURL)
	s.Require().ErrorContains(err, "403")
	s.Require().ErrorContains(err, "authenticated principal")
	s.Empty(stdout)
	s.Require().Len(s.fetchDatasetOperatorHolds(dataset, "active").Holds, 1,
		"refused release must leave the circuit closed")
}

// Explicit metric identities are observable even before a dataset is declared.
// This is the real marker path's free history for assertions added later.
func (s *IntegrationTestSuite) TestDataAssertionsDatasetUnregisteredMetricReads() {
	s.requireDataAssertionsLane()
	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("integration-operator-unregistered-%d", suffix)
	dataset := fmt.Sprintf("warehouse/unregistered-%d", suffix)
	step, err := metricsProducerStepForDataset("load", dataset, map[string]any{"rowCount": 1700})
	s.Require().NoError(err)
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  cache: false
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
%s`, alias, step)
	producer := s.applyAssertionsJob(alias, manifest)
	s.runProducer(producer)
	var result datasetsvc.MetricsResult
	s.fetchDatasetOperatorJSON(datasetOperatorPath(dataset)+"/metrics?metric=rowCount", &result)
	s.Require().Len(result.Series, 1)
	s.Equal(dataset, result.Name)
	s.Equal(1700.0, result.Series[0].Value)
}
