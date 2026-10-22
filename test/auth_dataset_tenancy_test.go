//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Dataset tenancy via declared producers (identity-and-access B6), on the
// api-key auth lane. Jobs and runs are produced for real (applied manifests,
// container runs, the freshness evaluator); the catalog is written only to
// produce preconditions the server would refuse to create today: a legacy
// co-producer (the single-producer validation rejects a second producer, but
// pre-validation catalogs can hold one) and metric history emitted under
// another namespace before a move.

// datasetTenancyFixture tracks the jobs, keys and seeded rows a scenario
// creates, all removed by closeNamespacedRoutesFixture.
func (s *IntegrationTestSuite) newDatasetTenancyFixture() *nsRoutesFixture {
	s.T().Helper()
	return &nsRoutesFixture{stamp: time.Now().UnixNano(), catalogDB: s.openIntegrationCatalogDB()}
}

// namespacedProducerManifest is a never-firing job in namespace producing
// dataset (plus extra step YAML under datasets, e.g. a sources block in
// metadata via metadataExtra).
func namespacedProducerManifest(alias, namespace, dataset string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  namespace: %s
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: load
    image: busybox:1.36.1
    command: ["sh","-c","echo b6"]
    datasets:
      produces:
        - name: %s
`, alias, namespace, dataset)
}

// applyTenancyJob applies manifest with the lane's admin key and records the
// job for cleanup.
func (s *IntegrationTestSuite) applyTenancyJob(f *nsRoutesFixture, alias, manifest string, extraEnv ...string) *jobSummary {
	s.T().Helper()
	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	out, errOut, err := s.runCLIWithEnv(append([]string{"CAESIUM_FRESHNESS_ENABLED=true", "CAESIUM_DATA_ASSERTIONS_ENABLED=true"}, extraEnv...),
		"job", "apply", "--path", dir, "--server", s.caesiumURL)
	s.Require().NoError(err, "apply %s: %s %s", alias, out, errOut)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)
	f.jobs = append(f.jobs, job.ID)
	return job
}

// seedCoProducer declares, in the catalog, that job also produces dataset: a
// legacy multi-producer dataset the importer no longer accepts. It is removed
// before any later apply could trip over it.
func (s *IntegrationTestSuite) seedCoProducer(f *nsRoutesFixture, job *jobSummary, alias, dataset string) {
	s.T().Helper()
	id := uuid.NewString()
	now := time.Now().UTC()
	s.catalogExec(f, 1, `INSERT INTO dataset_declarations (id, job_id, job_alias, step_name, name, direction, created_at, updated_at) VALUES (?, ?, ?, 'load', ?, 'produces', ?, ?)`,
		id, job.ID, alias, dataset, now, now)
	f.undo = append(f.undo, func() { s.catalogExec(f, -1, `DELETE FROM dataset_declarations WHERE id = ?`, id) })
}

// datasetCLI runs a `caesium dataset` verb as token and returns stdout,
// stderr and the error (stdout captured separately so --json parses clean).
func (s *IntegrationTestSuite) datasetCLI(token string, args ...string) (string, string, error) {
	s.T().Helper()
	return s.runCLIWithEnv([]string{"CAESIUM_API_KEY=" + token}, append(append([]string{"dataset"}, args...), "--server", s.caesiumURL)...)
}

func (s *IntegrationTestSuite) listedDatasetNames(token string) map[string]bool {
	s.T().Helper()
	out, errOut, err := s.datasetCLI(token, "list", "--json", "--limit", "200")
	s.Require().NoError(err, errOut)
	var result struct {
		Datasets []struct {
			Name string `json:"name"`
		} `json:"datasets"`
		Total int `json:"total"`
	}
	s.Require().NoError(json.Unmarshal([]byte(out), &result), "stdout must be clean JSON: %s", out)
	names := map[string]bool{}
	for _, row := range result.Datasets {
		names[row.Name] = true
	}
	s.Require().LessOrEqual(len(result.Datasets), result.Total)
	return names
}

// TestAuthDatasetReadsFollowDeclaredProducers: a namespace-confined viewer
// lists and reads only datasets whose declared producer lives in its
// namespaces; a foreign dataset and a missing one answer the same 403; and a
// run-less freshness skip belongs to its producer's namespace.
func (s *IntegrationTestSuite) TestAuthDatasetReadsFollowDeclaredProducers() {
	s.requireAuthLane()
	s.requireFreshnessLane()
	f := s.newDatasetTenancyFixture()
	defer s.closeNamespacedRoutesFixture(f)
	mktData := fmt.Sprintf("b6.mkt.%d", f.stamp)
	finData := fmt.Sprintf("b6.fin.%d", f.stamp)
	freshData := fmt.Sprintf("b6.fresh.%d", f.stamp)
	source := fmt.Sprintf("b6.src.%d", f.stamp)
	eventType := fmt.Sprintf("b6.arrival.%d", f.stamp)
	mktAlias, finAlias, freshAlias := fmt.Sprintf("b6-mkt-%d", f.stamp), fmt.Sprintf("b6-fin-%d", f.stamp), fmt.Sprintf("b6-fresh-%d", f.stamp)
	s.applyTenancyJob(f, mktAlias, namespacedProducerManifest(mktAlias, "marketing", mktData))
	finJob := s.applyTenancyJob(f, finAlias, namespacedProducerManifest(finAlias, "finance", finData))
	// A marketing freshness job consuming an external, arrival-bound source:
	// the source has no producer, so it is cluster-only.
	freshJob := s.applyTenancyJob(f, freshAlias, freshnessDerivationManifest(freshAlias, "  namespace: marketing\n", source, freshData, eventType, "out-b6"))
	f.undo = append(f.undo, func() {
		for _, name := range []string{mktData, finData, freshData, source} {
			s.catalogExec(f, -1, `DELETE FROM dataset_states WHERE name = ?`, name)
			s.catalogExec(f, -1, `DELETE FROM dataset_derivations WHERE name = ?`, name)
		}
	})

	mktViewer := s.mintNamespacedKey("viewer", "marketing").Plaintext
	finViewer := s.mintNamespacedKey("viewer", "finance").Plaintext

	listed := s.listedDatasetNames(mktViewer)
	s.True(listed[mktData], "marketing lists its producer's dataset before any run")
	s.True(listed[freshData])
	s.False(listed[finData], "a finance dataset is not listed")
	s.False(listed[source], "a source nobody produces is cluster-only")
	listed = s.listedDatasetNames(finViewer)
	s.True(listed[finData])
	s.False(listed[mktData])
	s.False(listed[freshData])
	all := s.listedDatasetNames(s.authAPIKey)
	for _, name := range []string{mktData, finData, freshData, source} {
		s.True(all[name], "the cluster admin lists %s exactly as before", name)
	}

	// A foreign dataset and a missing one are indistinguishable.
	foreignStatus, foreignBody := s.nsRequest(http.MethodGet, "/v1/datasets/_/"+mktData, finViewer, "", nil)
	missingStatus, missingBody := s.nsRequest(http.MethodGet, "/v1/datasets/_/b6.missing."+uuid.NewString(), finViewer, "", nil)
	s.Equal(http.StatusForbidden, foreignStatus, foreignBody)
	s.Equal(foreignStatus, missingStatus)
	s.JSONEq(foreignBody, missingBody)
	s.requireNSStatus(http.StatusForbidden, http.MethodGet, "/v1/datasets/_/"+source, mktViewer, "")

	// The paused freshness job records a run-less skip on arrival.
	s.Require().Equal(http.StatusOK, s.setJobPaused(freshJob.ID, "pause"))
	time.Sleep(3 * time.Second)
	s.postEvent(fmt.Sprintf(`{"type":%q,"source":"b6","data":{"detail":{"kind":"orders","objects":[{"key":"b6-%d"}]}}}`, eventType, f.stamp))
	type derivations struct {
		Derivations []struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
			RunID    string `json:"run_id"`
		} `json:"derivations"`
		Total int `json:"total"`
	}
	readDerivations := func(token string) derivations {
		s.T().Helper()
		status, body := s.nsRequest(http.MethodGet, "/v1/datasets/_/"+freshData+"/derivations", token, "", nil)
		s.Require().Equal(http.StatusOK, status, body)
		var out derivations
		s.Require().NoError(json.Unmarshal([]byte(body), &out), body)
		return out
	}
	s.Require().Eventually(func() bool {
		for _, d := range readDerivations(mktViewer).Derivations {
			if d.Decision == "skipped_admission" && d.Reason == "job is paused" && d.RunID == "" {
				return true
			}
		}
		return false
	}, 90*time.Second, time.Second, "the marketing viewer must read its producer's paused skip")

	// Finance co-produces the dataset (a seeded legacy declaration): it may
	// now read the dataset, but not marketing's decisions.
	s.seedCoProducer(f, finJob, finAlias, freshData)
	finRead := readDerivations(finViewer)
	s.Zero(finRead.Total, "marketing's decisions are not finance's: %+v", finRead)
	out, errOut, err := s.datasetCLI(finViewer, "status", freshData, "--json")
	s.Require().NoError(err, errOut)
	var detail struct {
		Producing struct {
			Alias string `json:"alias"`
		} `json:"producing_job"`
	}
	s.Require().NoError(json.Unmarshal([]byte(out), &detail), out)
	s.Equal(finAlias, detail.Producing.Alias)
	s.NotContains(out, freshAlias, "finance never sees the marketing producer")
	s.NotContains(out, freshJob.ID)
	out, errOut, err = s.datasetCLI(mktViewer, "status", freshData, "--json")
	s.Require().NoError(err, errOut)
	s.NotContains(out, finAlias)
	s.NotContains(out, finJob.ID)
	s.NotZero(readDerivations(s.authAPIKey).Total, "the cluster admin reads every decision")
}

// holdProducerManifestIn is holdProducerManifest in a namespace, with a
// manual release so a clean run never clears the hold under the scenario.
func holdProducerManifestIn(alias, namespace, dataset, step string) string {
	manifest := holdProducerManifest(alias, dataset, step, "manual")
	return strings.Replace(manifest, "  cache: false\n", "  cache: false\n  namespace: "+namespace+"\n", 1)
}

// TestAuthDatasetHoldReleaseNeedsEveryProducer: a real breach opens a hold in
// marketing; with finance as a (legacy) co-producer, finance reads the hold
// without marketing's holder or evidence and reads only the metric history
// persisted under finance; a marketing operator cannot release it (operator
// is needed in every producer namespace) and the cluster admin can.
func (s *IntegrationTestSuite) TestAuthDatasetHoldReleaseNeedsEveryProducer() {
	s.requireAuthLane()
	s.requireDataAssertionsLane()
	f := s.newDatasetTenancyFixture()
	defer s.closeNamespacedRoutesFixture(f)
	dataset := fmt.Sprintf("b6.held.%d", f.stamp)
	mktAlias, finAlias := fmt.Sprintf("b6-hold-mkt-%d", f.stamp), fmt.Sprintf("b6-hold-fin-%d", f.stamp)
	finJob := s.applyTenancyJob(f, finAlias, namespacedProducerManifest(finAlias, "finance", fmt.Sprintf("b6.finonly.%d", f.stamp)))
	f.undo = append(f.undo, func() {
		s.catalogExec(f, -1, `DELETE FROM dataset_holds WHERE name = ?`, dataset)
		s.catalogExec(f, -1, `DELETE FROM dataset_metrics WHERE name = ?`, dataset)
		s.catalogExec(f, -1, `DELETE FROM dataset_states WHERE name IN (?, ?)`, dataset, fmt.Sprintf("b6.finonly.%d", f.stamp))
	})

	// Finance history for the dataset: a finance run whose task run carries
	// three clean samples with a distinctive value, older than marketing's.
	finRunID := s.triggerRun(finJob.ID)
	s.Require().Equal("succeeded", s.awaitRun(finJob.ID, finRunID, runTimeout).Status)
	var finTaskRun string
	s.Require().NoError(f.catalogDB.QueryRowContext(s.T().Context(), `SELECT id FROM task_runs WHERE job_run_id = ? LIMIT 1`, finRunID).Scan(&finTaskRun))
	for i := range 3 {
		s.catalogExec(f, 1, `INSERT INTO dataset_metrics (id, task_run_id, namespace, name, metric, value, violated, created_at) VALUES (?, ?, '', ?, 'rowCount', 4242, false, ?)`,
			uuid.NewString(), finTaskRun, dataset, time.Now().UTC().Add(-time.Hour+time.Duration(i)*time.Minute))
	}

	// Marketing produces the dataset for real: one clean run, then a breach.
	clean, err := metricsProducerStepForDataset("load", dataset, map[string]any{"rowCount": 2000})
	s.Require().NoError(err)
	mktJob := s.applyTenancyJob(f, mktAlias, holdProducerManifestIn(mktAlias, "marketing", dataset, clean))
	var jobNamespace string
	s.Require().NoError(f.catalogDB.QueryRowContext(s.T().Context(), `SELECT namespace FROM jobs WHERE id = ?`, mktJob.ID).Scan(&jobNamespace))
	s.Require().Equal("marketing", jobNamespace)
	s.runProducer(mktJob)
	breach, err := metricsProducerStepForDataset("load", dataset, map[string]any{"rowCount": 5})
	s.Require().NoError(err)
	s.applyTenancyJob(f, mktAlias, holdProducerManifestIn(mktAlias, "marketing", dataset, breach))
	breachRun := s.runProducer(mktJob)
	var runNamespace string
	s.Require().NoError(f.catalogDB.QueryRowContext(s.T().Context(), `SELECT namespace FROM job_runs WHERE id = ?`, breachRun.ID).Scan(&runNamespace))
	s.Require().Equal("marketing", runNamespace, "the producing run is persisted in marketing")

	// Every apply is done: seed finance as a legacy co-producer.
	s.seedCoProducer(f, finJob, finAlias, dataset)
	mktViewer := s.mintNamespacedKey("viewer", "marketing").Plaintext
	finViewer := s.mintNamespacedKey("viewer", "finance").Plaintext
	mktOperator := s.mintNamespacedKey("operator", "marketing").Plaintext

	type holdRow struct {
		ID         string          `json:"id"`
		Name       string          `json:"name"`
		Status     string          `json:"status"`
		HeldBy     string          `json:"held_by_job_alias"`
		HeldByRun  string          `json:"held_by_run_id"`
		Violations json.RawMessage `json:"violations"`
		Impact     json.RawMessage `json:"impact"`
	}
	holdOf := func(token string) (holdRow, string) {
		s.T().Helper()
		out, errOut, err := s.datasetCLI(token, "holds", "--json", "--status", "all", "--limit", "200")
		s.Require().NoError(err, errOut)
		var result struct {
			Holds []holdRow `json:"holds"`
		}
		s.Require().NoError(json.Unmarshal([]byte(out), &result), "stdout must be clean JSON: %s", out)
		for _, hold := range result.Holds {
			if hold.Name == dataset {
				return hold, out
			}
		}
		s.T().Fatalf("no hold on %s in %s", dataset, out)
		return holdRow{}, out
	}
	mktHold, _ := holdOf(mktViewer)
	s.Equal(mktAlias, mktHold.HeldBy)
	s.Equal(breachRun.ID, mktHold.HeldByRun)
	s.NotEmpty(mktHold.Violations)
	s.Empty(mktHold.Impact, "the impact cone is never shown to a confined reader")
	finHold, finOut := holdOf(finViewer)
	s.Equal(mktHold.ID, finHold.ID)
	s.Empty(finHold.HeldBy, "marketing's holder is not finance's to see")
	s.Empty(finHold.HeldByRun)
	s.Empty(finHold.Violations, "the breach evidence carries marketing's baseline")
	s.NotContains(finOut, mktAlias)
	s.NotContains(finOut, breachRun.ID)

	type metrics struct {
		Series []struct {
			Value      float64 `json:"value"`
			InBaseline bool    `json:"in_baseline"`
		} `json:"series"`
		Total    int `json:"total"`
		Baseline struct {
			Samples int       `json:"samples"`
			Median  float64   `json:"median"`
			Values  []float64 `json:"values"`
		} `json:"baseline"`
	}
	readMetrics := func(token string) metrics {
		s.T().Helper()
		status, body := s.nsRequest(http.MethodGet, "/v1/datasets/_/"+dataset+"/metrics?metric=rowCount", token, "", nil)
		s.Require().Equal(http.StatusOK, status, body)
		var out metrics
		s.Require().NoError(json.Unmarshal([]byte(body), &out), body)
		return out
	}
	fin := readMetrics(finViewer)
	s.Equal(3, fin.Total, "finance reads only the samples persisted under finance")
	s.Len(fin.Series, 3)
	for _, sample := range fin.Series {
		s.InDelta(4242, sample.Value, 0.001)
		s.True(sample.InBaseline)
	}
	s.Equal(3, fin.Baseline.Samples)
	s.InDelta(4242, fin.Baseline.Median, 0.001)
	mkt := readMetrics(mktViewer)
	s.Equal(2, mkt.Total, "marketing reads its clean and breaching samples, not finance's")
	s.Require().Len(mkt.Series, 2)
	s.InDelta(2000, mkt.Series[0].Value, 0.001)
	s.True(mkt.Series[0].InBaseline)
	s.InDelta(5, mkt.Series[1].Value, 0.001)
	s.False(mkt.Series[1].InBaseline, "the rejected sample never joins the baseline")
	s.Equal(1, mkt.Baseline.Samples)
	s.Equal([]float64{2000}, mkt.Baseline.Values)
	s.NotContains(fmt.Sprint(mkt.Baseline.Values), "4242")
	s.Equal(5, readMetrics(s.authAPIKey).Total, "the cluster admin reads every sample")

	// Release needs operator in every producer namespace.
	_, errOut, err := s.datasetCLI(mktOperator, "release", dataset, "--reason", "b6 marketing only", "--json")
	s.Require().Error(err, "a marketing operator must not release a hold finance co-produces")
	s.Contains(errOut, "403")
	after, _ := holdOf(s.authAPIKey)
	s.Equal("active", after.Status, "a refused release changes nothing")
	out, errOut, err := s.datasetCLI(s.authAPIKey, "release", dataset, "--reason", "b6 cluster operator", "--json")
	s.Require().NoError(err, errOut)
	s.Contains(out, `"released"`)
	after, _ = holdOf(s.authAPIKey)
	s.Equal("released", after.Status)
}

// consumingProducerManifest is a never-firing job in namespace producing
// dataset with watermark value; with source set it also consumes that
// external, arrival-bound source.
func consumingProducerManifest(alias, namespace, dataset, value, source, eventType string) string {
	sources, consumes := "", ""
	if source != "" {
		sources = fmt.Sprintf(`  datasets:
    sources:
      - name: %s
        expectedEvery: 24h
        external: true
        arrival:
          event:
            type: %s
            filter:
              detail.kind: orders
          watermark: "$.detail.objects[0].key"
`, source, eventType)
		consumes = fmt.Sprintf(`      consumes:
        - %s
`, source)
	}
	return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  namespace: %s
  cache: false
%strigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: load
    image: alpine:3.23
    command: ["sh","-c","echo \"##caesium::output {\\\"wm\\\":\\\"%s\\\"}\""]
    datasets:
%s      produces:
        - name: %s
          watermark:
            key: wm
`, alias, namespace, sources, value, consumes, dataset)
}

// TestAuthDatasetConsumedInputsStayWithTheirRun: a finance producer consumes
// an input and runs; it then moves to marketing without inputs and runs
// again. The dataset's last run is marketing's, but the input snapshot is
// still the finance run's, so a marketing viewer must not see it.
func (s *IntegrationTestSuite) TestAuthDatasetConsumedInputsStayWithTheirRun() {
	s.requireAuthLane()
	s.requireFreshnessLane()
	f := s.newDatasetTenancyFixture()
	defer s.closeNamespacedRoutesFixture(f)
	dataset, source := fmt.Sprintf("b6.consumed.%d", f.stamp), fmt.Sprintf("b6.input.%d", f.stamp)
	eventType := fmt.Sprintf("b6.input.arrival.%d", f.stamp)
	alias := fmt.Sprintf("b6-consumer-%d", f.stamp)
	f.undo = append(f.undo, func() {
		for _, name := range []string{dataset, source} {
			s.catalogExec(f, -1, `DELETE FROM dataset_states WHERE name = ?`, name)
			s.catalogExec(f, -1, `DELETE FROM dataset_derivations WHERE name = ?`, name)
		}
	})
	type stateBody struct {
		State struct {
			Watermark string          `json:"watermark"`
			LastRunID string          `json:"last_run_id"`
			Consumed  json.RawMessage `json:"consumed_watermarks"`
		} `json:"state"`
	}
	read := func(token, name string) stateBody {
		s.T().Helper()
		status, body := s.nsRequest(http.MethodGet, "/v1/datasets/_/"+name, token, "", nil)
		s.Require().Equal(http.StatusOK, status, body)
		var out stateBody
		s.Require().NoError(json.Unmarshal([]byte(body), &out), body)
		return out
	}

	job := s.applyTenancyJob(f, alias, consumingProducerManifest(alias, "finance", dataset, "v1", source, eventType))
	s.postEvent(fmt.Sprintf(`{"type":%q,"source":"b6","data":{"detail":{"kind":"orders","objects":[{"key":"in-%d"}]}}}`, eventType, f.stamp))
	s.Require().Eventually(func() bool { return read(s.authAPIKey, source).State.Watermark != "" }, 60*time.Second, time.Second,
		"the arrival must advance the input")
	finRun := s.runProducer(job)
	s.Require().Eventually(func() bool { return read(s.authAPIKey, dataset).State.LastRunID == finRun.ID }, 60*time.Second, time.Second)
	s.Require().Contains(string(read(s.authAPIKey, dataset).State.Consumed), source, "precondition: the finance run's input snapshot")

	s.applyTenancyJob(f, alias, consumingProducerManifest(alias, "marketing", dataset, "v2", "", ""))
	mktRun := s.runProducer(job)
	s.Require().Eventually(func() bool { return read(s.authAPIKey, dataset).State.LastRunID == mktRun.ID }, 60*time.Second, time.Second)
	admin := read(s.authAPIKey, dataset)
	s.Require().Contains(string(admin.State.Consumed), source, "precondition: a run without inputs leaves the snapshot in place")

	mktViewer := s.mintNamespacedKey("viewer", "marketing").Plaintext
	mkt := read(mktViewer, dataset)
	s.Equal(mktRun.ID, mkt.State.LastRunID, "the last run is marketing's")
	s.Empty(mkt.State.Consumed, "the input snapshot is the finance run's")
	out, errOut, err := s.datasetCLI(mktViewer, "list", "--json", "--limit", "200")
	s.Require().NoError(err, errOut)
	s.Contains(out, dataset)
	s.NotContains(out, source, "neither the list nor the detail names finance's input")
}
