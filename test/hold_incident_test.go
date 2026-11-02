//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/incident"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
)

type holdIncident struct {
	ID                string          `json:"id"`
	JobID             string          `json:"job_id"`
	RunID             string          `json:"run_id"`
	TaskName          string          `json:"task_name"`
	Namespace         string          `json:"namespace"`
	Class             string          `json:"class"`
	Status            string          `json:"status"`
	OccurrenceCount   int             `json:"occurrence_count"`
	LastError         string          `json:"last_error"`
	ResolutionSummary string          `json:"resolution_summary"`
	AllowedJobs       []string        `json:"allowed_jobs"`
	Evidence          json.RawMessage `json:"evidence"`
}

type holdIncidentEvidence struct {
	HoldID           string `json:"hold_id"`
	DatasetNamespace string `json:"dataset_namespace"`
	Dataset          string `json:"dataset"`
	Violations       []struct {
		Assertion       string   `json:"assertion"`
		Observed        *float64 `json:"observed"`
		Bound           *float64 `json:"bound"`
		BaselineMedian  *float64 `json:"baseline_median"`
		BaselineSamples int      `json:"baseline_samples"`
		BaselineScope   string   `json:"baseline_scope"`
	} `json:"violations"`
	Impact struct {
		Downstream []redactedImpactNode `json:"downstream"`
	} `json:"impact"`
}

// holdIncidents lists the job's data_quality_hold incidents, oldest first,
// through the CLI: its stdout must stay clean JSON.
func (s *IntegrationTestSuite) holdIncidents(jobID string) []holdIncident {
	s.T().Helper()
	args := []string{"incident", "list", "--json", "--server", s.caesiumURL, "--job-id", jobID, "--class", "data_quality_hold"}
	if key := firstEnv("CAESIUM_INCIDENT_API_KEY", "CAESIUM_API_KEY", "CAESIUM_E2E_AUTH_ADMIN_KEY"); key != "" {
		args = append(args, "--api-key", key)
	}
	out, err := s.runCLIStdout(args...)
	s.Require().NoError(err, "caesium incident list failed:\n%s", out)
	var list struct {
		Incidents []holdIncident `json:"incidents"`
	}
	s.Require().NoError(json.Unmarshal([]byte(out), &list), "stdout was not clean JSON:\n%s", out)
	// The list is newest-first.
	for i, j := 0, len(list.Incidents)-1; i < j; i, j = i+1, j-1 {
		list.Incidents[i], list.Incidents[j] = list.Incidents[j], list.Incidents[i]
	}
	return list.Incidents
}

func (s *IntegrationTestSuite) awaitHoldIncidents(jobID string, want int) []holdIncident {
	s.T().Helper()
	var got []holdIncident
	s.Require().Eventually(func() bool {
		got = s.holdIncidents(jobID)
		return len(got) >= want
	}, 60*time.Second, 500*time.Millisecond, "expected %d data_quality_hold incident(s) for job %s", want, jobID)
	s.Require().Len(got, want)
	return got
}

// incidentSubscriberFence returns once the incident subscriber has consumed
// everything published before the call. Events reach it in order, so a later
// failure closing on a later success proves the earlier events were handled:
// without this, "still open" would pass on a subscriber that has not got there.
func (s *IntegrationTestSuite) incidentSubscriberFence(alias string) {
	s.T().Helper()
	run := func(exitCode int) {
		definition := failingJobDefinition(alias, s.engineType)
		definition.Steps[0].Command = []string{"sh", "-c", fmt.Sprintf("echo %s; exit %d", alias, exitCode)}
		s.applyDefinition(definition)
		job := s.requireJobByAlias(alias)
		s.awaitRun(job.ID, s.triggerRun(job.ID), runTimeout)
	}
	run(1)
	job := s.requireJobByAlias(alias)
	fence := s.awaitNamespaceLifecycleIncident(job.ID, "default")
	run(0)
	s.awaitIncidentStatus(fence.ID, "closed", 60*time.Second)
}

// holdBaselineSamples is CAESIUM_BASELINE_MIN_SAMPLES's default: fewer clean
// samples and deltaFromBaseline only seeds.
const holdBaselineSamples = 5

// holdProducerIn is holdProducerManifest in a tenant namespace, with a
// deltaFromBaseline assertion beside its absolute floor.
func (s *IntegrationTestSuite) holdProducerIn(namespace, alias, dataset string, rowCount float64) *jobSummary {
	s.T().Helper()
	step, err := metricsProducerStepForDataset("load", dataset, map[string]any{"rowCount": rowCount})
	s.Require().NoError(err)
	manifest := holdProducerManifest(alias, dataset, step, "manual")
	manifest = strings.Replace(manifest, "  cache: false", "  namespace: "+namespace+"\n  cache: false", 1)
	manifest = strings.Replace(manifest, "rowCount: {min: 1000}", "rowCount: {min: 1000, deltaFromBaseline: 50%}", 1)
	return s.applyAssertionsJob(alias, manifest)
}

// TestHoldOpensDataQualityHoldIncident drives data-circuit-breaker F1 through
// the real surface: a real container emits a bad metric, the run goes green, the
// dataset is held, and exactly one incident opens. It survives a green rerun of
// the same job, closes on the release and not before, and the next hold on the
// same dataset opens a second incident. Its evidence carries the real baseline
// the breach was judged against and names a downstream node of another tenant
// only as a placeholder, and the agent reads the same through its scoped bundle.
func (s *IntegrationTestSuite) TestHoldOpensDataQualityHoldIncident() {
	s.requireAuthLane()
	s.requirePolicyFixture()
	s.requireDataAssertionsLane()

	stamp := time.Now().UnixNano()
	producerAlias := fmt.Sprintf("f1-producer-%d", stamp)
	ownConsumer := fmt.Sprintf("f1-own-consumer-%d", stamp)
	foreignConsumer := fmt.Sprintf("f1-foreign-consumer-%d", stamp)
	dataset := fmt.Sprintf("f1.orders.%d", stamp)
	ownOutput := fmt.Sprintf("f1-own-rollup-%d", stamp)
	foreignOutput := fmt.Sprintf("f1-foreign-rollup-%d", stamp)
	s.cleanupInOrder(foreignConsumer, ownConsumer, producerAlias, producerAlias+"-fence")

	// The lineage the cone reads has to exist before the hold opens: a clean
	// producer run, then one consumer in the producer's tenant and one outside.
	producer := s.holdProducerIn("finance", producerAlias, dataset, 5000)
	for range holdBaselineSamples {
		s.runProducer(producer)
	}
	s.applyDefinition(s.namespacedJobDefinition(ownConsumer, "finance"))
	s.applyDefinition(s.namespacedJobDefinition(foreignConsumer, "marketing"))
	s.runToSuccess(ownConsumer)
	s.runToSuccess(foreignConsumer)
	s.seedObservedLineage(ownConsumer, "", dataset, "input")
	s.seedObservedLineage(ownConsumer, "", ownOutput, "output")
	s.seedObservedLineage(foreignConsumer, "", dataset, "input")
	s.seedObservedLineage(foreignConsumer, "", foreignOutput, "output")
	s.Empty(s.holdIncidents(producer.ID), "a clean run opens nothing")

	// The breach: the producing run is green and the dataset is held.
	producer = s.holdProducerIn("finance", producerAlias, dataset, 12)
	breach := s.runProducer(producer)

	first := s.awaitHoldIncidents(producer.ID, 1)[0]
	s.Equal("data_quality_hold", first.Class)
	s.Equal("open", first.Status)
	s.Equal("finance", first.Namespace, "the incident belongs to the producing job's tenant")
	s.Equal(breach.ID, first.RunID)
	s.Equal("load", first.TaskName)
	s.Equal(1, first.OccurrenceCount)
	s.Contains(first.LastError, dataset)
	s.Contains(first.AllowedJobs, ownConsumer, "the clean runs' recorded output finished before the hold, so it stays trusted")
	s.NotContains(first.AllowedJobs, foreignConsumer, "the allowlist stops at the incident's namespace")

	var evidence holdIncidentEvidence
	s.Require().NoError(json.Unmarshal(first.Evidence, &evidence), string(first.Evidence))
	s.Equal("", evidence.DatasetNamespace, "the dataset's namespace is a data axis, not the tenant")
	s.Equal(dataset, evidence.Dataset)
	s.Require().NotEmpty(evidence.HoldID)
	s.Require().Len(evidence.Violations, 2)
	s.assertHoldVerdicts(evidence)

	// The hold's own cone names the foreign consumer, or the assertions below
	// would pass on a fixture that never reached it.
	var holds struct {
		Holds []struct {
			ID     string          `json:"id"`
			Impact json.RawMessage `json:"impact"`
		} `json:"holds"`
	}
	s.getJSON("/v1/datasets/holds?name="+url.QueryEscape(dataset), &holds)
	s.Require().Len(holds.Holds, 1)
	s.Require().Equal(holds.Holds[0].ID, evidence.HoldID)
	s.Require().Contains(string(holds.Holds[0].Impact), foreignOutput)
	s.Require().Contains(string(holds.Holds[0].Impact), foreignConsumer)

	s.NotContains(string(first.Evidence), foreignOutput, "the evidence must not name another tenant's dataset")
	s.NotContains(string(first.Evidence), foreignConsumer, "the evidence must not name another tenant's job")
	var names, kinds []string
	for _, node := range evidence.Impact.Downstream {
		names = append(names, node.DatasetName)
		kinds = append(kinds, node.Kind)
	}
	s.Contains(names, ownOutput, "a node in the producer's tenant is shown whole")
	s.Contains(kinds, "external", "a foreign node stays in the cone as a placeholder")

	// The agent reads all of it through its own scoped surface.
	token := s.mintAgentSessionToken(first.ID, producerAlias)
	status, raw := s.requestWithKey(http.MethodGet, "/v1/agent/incidents/"+first.ID+"/bundle", token, nil)
	s.Require().Equal(http.StatusOK, status, raw)
	var bundle incident.Bundle
	s.Require().NoError(json.Unmarshal([]byte(raw), &bundle), raw)
	s.Equal(first.ID, bundle.Incident.ID.String())
	s.Equal("data_quality_hold", bundle.Classification.Class)
	var bundled holdIncidentEvidence
	s.Require().NoError(json.Unmarshal(bundle.Classification.Evidence, &bundled), raw)
	s.Equal(evidence.HoldID, bundled.HoldID)
	s.assertHoldVerdicts(bundled)
	var bundledNames, bundledKinds []string
	for _, node := range bundled.Impact.Downstream {
		bundledNames = append(bundledNames, node.DatasetName)
		bundledKinds = append(bundledKinds, node.Kind)
	}
	s.Contains(bundledNames, ownOutput)
	s.Contains(bundledKinds, "external")
	s.NotContains(raw, foreignOutput, "the bundle must not name another tenant's dataset")
	s.NotContains(raw, foreignConsumer, "the bundle must not name another tenant's job")
	// The task succeeded: nothing of it may read as the failure.
	s.Contains(bundle.Failure.Error, dataset, "the failure text is the verdict")
	s.Empty(bundle.Failure.LogTail)
	s.Empty(bundle.Failure.Result)
	s.Nil(bundle.Failure.ExitCode)
	s.Empty(bundle.Failure.Image)

	// A green rerun of a release: manual hold changes nothing. The producing
	// run's own task_succeeded and run_completed are behind the same fence.
	producer = s.holdProducerIn("finance", producerAlias, dataset, 5000)
	s.runProducer(producer)
	s.incidentSubscriberFence(producerAlias + "-fence")
	stillOpen := s.holdIncidents(producer.ID)
	s.Require().Len(stillOpen, 1, "exactly one incident per hold")
	s.Equal(first.ID, stillOpen[0].ID)
	s.Equal("open", stillOpen[0].Status, "a hold incident must not be remediated while its hold is active")

	body, err := json.Marshal(map[string]any{"reason": "source backfilled"})
	s.Require().NoError(err)
	resp, err := s.doJSONRequest(http.MethodPost,
		fmt.Sprintf("%v/v1/datasets/holds/%s/release", s.caesiumURL, evidence.HoldID), bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	s.awaitIncidentStatus(first.ID, "closed", 60*time.Second)
	closed := s.holdIncidents(producer.ID)
	s.Require().Len(closed, 1)
	s.Contains(closed[0].ResolutionSummary, "manual_ack")

	// A new breach is a new hold and a new incident, whatever the cooldown says.
	producer = s.holdProducerIn("finance", producerAlias, dataset, 7)
	second := s.runProducer(producer)
	incidents := s.awaitHoldIncidents(producer.ID, 2)
	s.Equal(first.ID, incidents[0].ID)
	s.Equal("closed", incidents[0].Status)
	s.NotEqual(first.ID, incidents[1].ID)
	s.Equal("open", incidents[1].Status)
	s.Equal(second.ID, incidents[1].RunID)
}

// TestHoldIncidentCollapsesForeignImpactForConfinedReader reads a hold's incident
// as a viewer confined to the producer's namespace, with a consumer of the dataset
// in another namespace. The lineage is real, from real runs, and the scenario needs
// no policy file and no catalog connection, so it also runs on the coverage auth
// lane: a hold with no recorded downstream never reaches the stored-cone code.
func (s *IntegrationTestSuite) TestHoldIncidentCollapsesForeignImpactForConfinedReader() {
	s.requireAuthLane()
	s.requireDataAssertionsLane()

	stamp := time.Now().UnixNano()
	producerAlias := fmt.Sprintf("f1-cone-producer-%d", stamp)
	ownConsumer := fmt.Sprintf("f1-cone-own-consumer-%d", stamp)
	foreignConsumer := fmt.Sprintf("f1-cone-other-consumer-%d", stamp)
	dataset := fmt.Sprintf("f1.cone.%d", stamp)
	ownOutput := fmt.Sprintf("f1-cone-own-rollup-%d", stamp)
	foreignOutput := fmt.Sprintf("f1-cone-other-rollup-%d", stamp)
	s.cleanupInOrder(foreignConsumer, ownConsumer, producerAlias)
	viewer := s.mintNamespacedKey("viewer", "finance")
	outsider := s.mintNamespacedKey("viewer", "marketing")

	// The cone is frozen when the hold opens, so the lineage comes first: a clean
	// producer run, then a run of each consumer.
	producer := s.holdProducerIn("finance", producerAlias, dataset, 5000)
	s.runProducer(producer)
	consumes := []schema.ConsumedDataset{{Name: dataset}}
	s.applyDefinition(s.datasetJob(ownConsumer, "finance", consumes, []schema.ProducedDataset{{Name: ownOutput}}))
	s.applyDefinition(s.datasetJob(foreignConsumer, "marketing", consumes, []schema.ProducedDataset{{Name: foreignOutput}}))
	s.runToSuccess(ownConsumer)
	s.runToSuccess(foreignConsumer)
	s.Require().Eventually(func() bool {
		var whole redactedImpact
		if s.tryGetJSON(impactPath(dataset), &whole) != nil {
			return false
		}
		seen := map[string]bool{}
		for _, node := range whole.Downstream {
			seen[node.DatasetName] = true
		}
		return seen[ownOutput] && seen[foreignOutput]
	}, 60*time.Second, 500*time.Millisecond, "lineage never linked %s to both consumers", dataset)

	producer = s.holdProducerIn("finance", producerAlias, dataset, 12)
	s.runProducer(producer)
	found := s.awaitHoldIncidents(producer.ID, 1)[0]

	// The hold's own cone names the foreign consumer, or the assertions below
	// would pass on a fixture that never reached it.
	var holds struct {
		Holds []struct {
			Impact json.RawMessage `json:"impact"`
		} `json:"holds"`
	}
	s.getJSON("/v1/datasets/holds?name="+url.QueryEscape(dataset), &holds)
	s.Require().Len(holds.Holds, 1)
	s.Require().Contains(string(holds.Holds[0].Impact), foreignOutput)
	s.Require().Contains(string(holds.Holds[0].Impact), foreignConsumer)

	status, _, body := s.nsGet("/v1/incidents/"+found.ID, viewer.Plaintext)
	s.Require().Equal(http.StatusOK, status, body)
	var detail struct {
		Incident holdIncident `json:"incident"`
	}
	s.Require().NoError(json.Unmarshal([]byte(body), &detail), body)
	s.Equal("finance", detail.Incident.Namespace)
	var evidence holdIncidentEvidence
	s.Require().NoError(json.Unmarshal(detail.Incident.Evidence, &evidence), body)
	var names, kinds []string
	for _, node := range evidence.Impact.Downstream {
		names = append(names, node.DatasetName)
		kinds = append(kinds, node.Kind)
	}
	s.Contains(names, ownOutput, "a node in the reader's namespace is shown whole: %s", body)
	s.Contains(kinds, "external", "the foreign node stays in the cone as a placeholder: %s", body)
	s.NotContains(body, foreignOutput, "the reader must not see another namespace's dataset")
	s.NotContains(body, foreignConsumer, "the reader must not see another namespace's job")
	s.NotContains(body, "marketing", "the reader must not see another namespace's name")

	status, _, body = s.nsGet("/v1/incidents/"+found.ID, outsider.Plaintext)
	s.Equal(http.StatusForbidden, status, "the incident belongs to the producer's namespace: %s", body)
}

// assertHoldVerdicts checks the breach of rowCount 12 against both assertions:
// the absolute floor, and the delta from the five real samples of 5000 before it.
func (s *IntegrationTestSuite) assertHoldVerdicts(evidence holdIncidentEvidence) {
	s.T().Helper()
	var sawMin, sawDelta bool
	for _, violation := range evidence.Violations {
		s.Require().NotNil(violation.Observed)
		s.InDelta(12, *violation.Observed, 0.001)
		s.Require().NotNil(violation.Bound)
		switch violation.Assertion {
		case "min":
			sawMin = true
			s.InDelta(1000, *violation.Bound, 0.001)
		case "deltaFromBaseline":
			sawDelta = true
			s.Require().NotNil(violation.BaselineMedian, "a real baseline must reach the evidence")
			s.InDelta(5000, *violation.BaselineMedian, 0.001)
			s.GreaterOrEqual(violation.BaselineSamples, holdBaselineSamples)
			s.Equal("namespace", violation.BaselineScope)
		}
	}
	s.True(sawMin, "%+v", evidence.Violations)
	s.True(sawDelta, "%+v", evidence.Violations)
}

// TestHoldIncidentFollowsHoldStateWithoutEvents is the leader's sweep on a live
// server. A follower consuming the hold's events is not reproducible on this
// single-node lane, so the scenario writes the hold row directly, which publishes
// nothing: to the subscriber that is what a lost event looks like. The sweep must
// open the incident, leave it open, and close it when the row is released.
func (s *IntegrationTestSuite) TestHoldIncidentFollowsHoldStateWithoutEvents() {
	s.requireAuthLane()
	s.requirePolicyFixture()
	s.requireDataAssertionsLane()

	stamp := time.Now().UnixNano()
	alias := fmt.Sprintf("f1-sweep-producer-%d", stamp)
	dataset := fmt.Sprintf("f1.sweep.%d", stamp)
	s.cleanupInOrder(alias)
	producer := s.holdProducerIn("finance", alias, dataset, 5000)
	run := s.runProducer(producer)

	catalog := s.openIntegrationCatalogDB()
	defer func() { s.NoError(catalog.Close()) }()
	holdID := uuid.NewString()
	opened := time.Now().UTC().Add(-time.Hour)
	_, err := catalog.ExecContext(s.T().Context(),
		`INSERT INTO dataset_holds (id, namespace, name, status, active_key, reason, held_by_job_id, held_by_job_alias, held_by_run_id, held_by_step_name, violations, occurrence_count, opened_at, created_at, updated_at)
		 VALUES (?, '', ?, 'active', ?, 'min', ?, ?, ?, 'load', ?, 1, ?, ?, ?)`,
		holdID, dataset, "/"+dataset, producer.ID, alias, run.ID,
		`[{"dataset":"`+dataset+`","metric":"rowCount","assertion":"min","observed":12,"bound":1000,"message":"rowCount 12 is below min 1000"}]`,
		opened, opened, opened)
	s.Require().NoError(err)

	found := s.awaitHoldIncidents(producer.ID, 1)[0]
	s.Equal("open", found.Status)
	s.Equal("finance", found.Namespace)
	s.Equal(run.ID, found.RunID)
	s.Contains(string(found.Evidence), holdID)

	_, err = catalog.ExecContext(s.T().Context(),
		`UPDATE dataset_holds SET status = 'released', active_key = NULL, released_by = 'system', release_reason = 'clean_run' WHERE id = ?`, holdID)
	s.Require().NoError(err)

	s.awaitIncidentStatus(found.ID, "closed", 60*time.Second)
	closed := s.holdIncidents(producer.ID)
	s.Require().Len(closed, 1, "a hold has one incident, and an ended hold opens none")
	s.Contains(closed[0].ResolutionSummary, "clean_run")
}
