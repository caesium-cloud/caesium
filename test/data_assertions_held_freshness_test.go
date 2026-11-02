//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	datasetsvc "github.com/caesium-cloud/caesium/api/rest/service/dataset"
	"github.com/stretchr/testify/require"
)

// runCLIWithDataAssertionsAndFreshness runs the CLI with both feature flags the
// client-side manifest validation gates on: `trigger.type: freshness` needs
// CAESIUM_FRESHNESS_ENABLED and a produced dataset's assertions need
// CAESIUM_DATA_ASSERTIONS_ENABLED. The lanes set them on the SERVER; the
// test-runner container is a separate process.
func (s *IntegrationTestSuite) runCLIWithDataAssertionsAndFreshness(args ...string) {
	s.T().Helper()
	cmd := exec.CommandContext(s.T().Context(), s.cliPath, args...)
	cmd.Dir = s.projectRoot
	cmd.Env = append(os.Environ(), "CAESIUM_DATA_ASSERTIONS_ENABLED=true", "CAESIUM_FRESHNESS_ENABLED=true")
	output, err := cmd.CombinedOutput()
	require.NoError(s.T(), err, "cli %v failed: %s", args, string(output))
}

func (s *IntegrationTestSuite) applyHeldFreshnessJob(alias, manifest string) *jobSummary {
	s.T().Helper()
	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLIWithDataAssertionsAndFreshness("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)
	return job
}

// heldFreshnessProducerManifest is a cron producer of `dataset` with a freshness
// SLO (so the evaluator tracks it) and a rowCount floor under onViolation: hold.
func heldFreshnessProducerManifest(alias, dataset, step string) string {
	return heldProducerManifestWithSLO(alias, dataset, step, "          freshness: 1h\n")
}

// heldProducerManifestWithSLO takes the produces entry's freshness line
// verbatim ("" drops the SLO).
func heldProducerManifestWithSLO(alias, dataset, step, freshnessLine string) string {
	return fmt.Sprintf(`
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
%s    datasets:
      produces:
        - name: %s
%s          assertions:
            rowCount: {min: 1000}
          onViolation: hold
          release: auto
`, alias, step, dataset, freshnessLine)
}

// heldFreshnessConsumerManifest is a purely data-derived (freshness-triggered)
// job that consumes `dataset` and produces `output` with a 1s SLO, so it is
// stale the moment its input is observed and would derive a run immediately
// unless the input is held.
func heldFreshnessConsumerManifest(alias, dataset, output, onUpstreamHold string) string {
	policy := ""
	if onUpstreamHold != "" {
		policy = "\n  onUpstreamHold: " + onUpstreamHold
	}
	return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  cache: false%s
trigger:
  type: freshness
  configuration: {}
steps:
  - name: refresh
    image: %s
    command: ["sh","-c","echo derived"]
    datasets:
      consumes:
        - %s
      produces:
        - name: %s
          freshness: 1s
`, alias, policy, metricsFixtureImage, dataset, output)
}

func (s *IntegrationTestSuite) fetchDatasetDetail(dataset string) datasetsvc.Detail {
	s.T().Helper()
	var detail datasetsvc.Detail
	s.fetchDatasetOperatorJSON(datasetOperatorPath(dataset), &detail)
	return detail
}

func (s *IntegrationTestSuite) datasetListContains(status, dataset string) bool {
	s.T().Helper()
	var list datasetsvc.ListResult
	s.fetchDatasetOperatorJSON("/v1/datasets?"+url.Values{"status": {status}, "limit": {"200"}}.Encode(), &list)
	for _, state := range list.Datasets {
		if state.Name == dataset {
			return true
		}
	}
	return false
}

// TestDataAssertionsHeldDatasetIsNotFreshAndBlocksDerivation is item F4 on the
// real surface: a producer breaches a `hold`-mode dataset on a green run, and
//
//   - GET /v1/datasets (list, detail, the status filter) and the CLI report the
//     dataset held with a reason naming the hold, never fresh, even though its
//     watermark is brand new;
//   - a freshness-triggered job downstream of it is NOT derived (no run row at
//     all, not even the hold-skipped one the admission gate would write), and
//     its own output reports stale-upstream naming the hold;
//   - after the holder's next clean run releases the hold, the status is honest
//     again with no manual step and the downstream job derives.
func (s *IntegrationTestSuite) TestDataAssertionsHeldDatasetIsNotFreshAndBlocksDerivation() {
	s.requireDataAssertionsLane()
	s.requireFreshnessLane()

	suffix := time.Now().UnixNano()
	producerAlias := fmt.Sprintf("integration-held-fresh-producer-%d", suffix)
	consumerAlias := fmt.Sprintf("integration-held-fresh-consumer-%d", suffix)
	// A slash is part of the declared Name and must survive every surface.
	dataset := fmt.Sprintf("warehouse/held-fresh-%d", suffix)
	output := fmt.Sprintf("integration.held.fresh.out.%d", suffix)

	// 1. A breaching producer run: it SUCCEEDS and holds the dataset. The
	// consumer is applied before the run, so its input has never been observed
	// (it cannot derive) until the producer's breach run captures it.
	breachStep, err := metricsProducerStepForDataset("load", dataset, map[string]any{"rowCount": 12})
	s.Require().NoError(err)
	producer := s.applyHeldFreshnessJob(producerAlias, heldFreshnessProducerManifest(producerAlias, dataset, breachStep))
	consumer := s.applyHeldFreshnessJob(consumerAlias, heldFreshnessConsumerManifest(consumerAlias, dataset, output, ""))
	// A monitoring consumer opts out of the gate: it is meant to run on held data.
	monitorAlias := fmt.Sprintf("integration-held-fresh-monitor-%d", suffix)
	monitorOutput := fmt.Sprintf("integration.held.fresh.monitor.%d", suffix)
	monitor := s.applyHeldFreshnessJob(monitorAlias, heldFreshnessConsumerManifest(monitorAlias, dataset, monitorOutput, "run"))
	s.runProducer(producer)

	// 2. The dataset reports held, not fresh.
	var detail datasetsvc.Detail
	s.Require().Eventually(func() bool {
		detail = s.fetchDatasetDetail(dataset)
		return detail.State.Status == "held"
	}, 60*time.Second, 500*time.Millisecond, "a held dataset must report status held")
	s.Contains(detail.State.Reason, "dataset_hold:/"+dataset, "the status reason must name the hold")
	s.Equal("active", detail.HoldStatus)
	s.True(s.datasetListContains("held", dataset), "GET /v1/datasets?status=held must list the held dataset")
	s.False(s.datasetListContains("fresh", dataset), "a held dataset must never list as fresh")
	var all datasetsvc.ListResult
	s.fetchDatasetOperatorJSON("/v1/datasets?limit=200", &all)
	for _, state := range all.Datasets {
		if state.Name == dataset {
			s.Equal("held", state.Status)
			s.Contains(state.Reason, "dataset_hold:/"+dataset)
		}
	}

	// Machine output is one clean JSON value on stdout, captured apart from stderr.
	stdout, err := s.runCLIStdout("dataset", "status", dataset, "--json", "--server", s.caesiumURL)
	s.Require().NoError(err)
	s.Require().True(json.Valid([]byte(stdout)), "stdout must be clean JSON: %s", stdout)
	var cliDetail datasetsvc.Detail
	s.Require().NoError(json.Unmarshal([]byte(stdout), &cliDetail))
	s.Equal("held", cliDetail.State.Status)
	s.Contains(cliDetail.State.Reason, "dataset_hold:/"+dataset)

	// 3. The freshness-triggered consumer is not derived. Wait for the
	// evaluator to have demonstrably looked at its output and refused for the
	// hold, so the "no runs" assertion is not vacuous.
	s.Require().Eventually(func() bool {
		var derivations datasetDerivationsTypedResponse
		if err := s.tryGetJSON("/v1/datasets/_/"+url.PathEscape(output)+"/derivations", &derivations); err != nil {
			return false
		}
		for _, d := range derivations.Derivations {
			if d.Decision == "skipped_upstream" && strings.Contains(d.Reason, "dataset_hold:/"+dataset) {
				return true
			}
		}
		return false
	}, 90*time.Second, time.Second, "the evaluator must refuse to derive off a held input and say why")
	// ...and the opted-out monitor is derived and runs, on the held input.
	s.Require().Eventually(func() bool {
		for _, run := range s.fetchRuns(monitor.ID) {
			if run.Status == "succeeded" && run.SkipReason == "" {
				return true
			}
		}
		return false
	}, 90*time.Second, time.Second, "metadata.onUpstreamHold: run must let a freshness-triggered job derive off a held input")
	s.Empty(s.fetchRuns(consumer.ID), "a freshness job downstream of a held dataset must not run - not even as a hold-skipped row")
	outDetail := s.fetchDatasetDetail(output)
	s.Equal("stale-upstream", outDetail.State.Status, "the consumer's own data is not poisoned; it is behind a held input")
	s.Contains(outDetail.State.Reason, "dataset_hold:/"+dataset)

	// 4. A clean run of the holder releases the hold (release: auto); the status
	// is honest again with no manual step, and the downstream job derives.
	cleanStep, err := metricsProducerStepForDataset("load", dataset, map[string]any{"rowCount": 5000})
	s.Require().NoError(err)
	producer = s.applyHeldFreshnessJob(producerAlias, heldFreshnessProducerManifest(producerAlias, dataset, cleanStep))
	clean := s.runProducer(producer)
	s.Empty(s.requireRunTaskByName(producer.ID, clean, "load").DataViolations)

	s.Require().Eventually(func() bool {
		detail = s.fetchDatasetDetail(dataset)
		return detail.State.Status == "fresh"
	}, 90*time.Second, 500*time.Millisecond, "after release the dataset must report its computed freshness again")
	s.NotContains(detail.State.Reason, "dataset_hold")
	s.Empty(detail.HoldStatus)
	s.False(s.datasetListContains("held", dataset), "a released dataset must not list as held")
	s.True(s.datasetListContains("fresh", dataset))

	s.Require().Eventually(func() bool {
		for _, run := range s.fetchRuns(consumer.ID) {
			if run.Status == "succeeded" {
				return true
			}
		}
		return false
	}, 120*time.Second, time.Second, "once the hold is released the downstream freshness job derives")
}

// TestDataAssertionsReleaseAfterDroppingTheSLOLeavesHeld proves a dataset does
// not stay `held` after its hold is released just because the producer stopped
// declaring a freshness SLO while it was held (per-dataset evaluation skips
// datasets with no SLO, so the release transaction itself has to clear it).
func (s *IntegrationTestSuite) TestDataAssertionsReleaseAfterDroppingTheSLOLeavesHeld() {
	s.requireDataAssertionsLane()
	s.requireFreshnessLane()

	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("integration-held-noslo-%d", suffix)
	dataset := fmt.Sprintf("integration.held.noslo.%d", suffix)

	breachStep, err := metricsProducerStepForDataset("load", dataset, map[string]any{"rowCount": 12})
	s.Require().NoError(err)
	producer := s.applyHeldFreshnessJob(alias, heldFreshnessProducerManifest(alias, dataset, breachStep))
	s.runProducer(producer)

	// include_hold=false skips the live-hold overlay, so this waits for the
	// evaluator to have PERSISTED `held` - the state a stale row would keep.
	s.Require().Eventually(func() bool {
		var persisted datasetsvc.Detail
		s.fetchDatasetOperatorJSON(datasetOperatorPath(dataset)+"?include_hold=false", &persisted)
		return persisted.State.Status == "held"
	}, 60*time.Second, 500*time.Millisecond, "the evaluator must persist held for an SLO-tracked dataset")

	// The producer drops its SLO and runs clean: the hold auto-releases.
	cleanStep, err := metricsProducerStepForDataset("load", dataset, map[string]any{"rowCount": 5000})
	s.Require().NoError(err)
	producer = s.applyHeldFreshnessJob(alias, heldProducerManifestWithSLO(alias, dataset, cleanStep, ""))
	clean := s.runProducer(producer)
	s.Empty(s.requireRunTaskByName(producer.ID, clean, "load").DataViolations)

	s.Require().Eventually(func() bool {
		var persisted datasetsvc.Detail
		s.fetchDatasetOperatorJSON(datasetOperatorPath(dataset)+"?include_hold=false", &persisted)
		return persisted.State.Status != "held"
	}, 90*time.Second, 500*time.Millisecond, "a released dataset with no SLO must leave held")
	detail := s.fetchDatasetDetail(dataset)
	s.Equal("unknown", detail.State.Status)
	s.NotContains(detail.State.Reason, "dataset_hold")
	s.Empty(detail.HoldStatus)
	s.False(s.datasetListContains("held", dataset), "?status=held must not list a released dataset")
}
