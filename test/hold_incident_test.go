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
		Assertion string   `json:"assertion"`
		Observed  *float64 `json:"observed"`
		Bound     *float64 `json:"bound"`
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

// holdProducerIn is holdProducerManifest in a tenant namespace.
func (s *IntegrationTestSuite) holdProducerIn(namespace, alias, dataset string, rowCount float64) *jobSummary {
	s.T().Helper()
	step, err := metricsProducerStepForDataset("load", dataset, map[string]any{"rowCount": rowCount})
	s.Require().NoError(err)
	manifest := strings.Replace(holdProducerManifest(alias, dataset, step, "manual"),
		"  cache: false", "  namespace: "+namespace+"\n  cache: false", 1)
	return s.applyAssertionsJob(alias, manifest)
}

// TestHoldOpensDataQualityHoldIncident drives data-circuit-breaker F1 through
// the real surface: a real container emits a bad metric, the run goes green, the
// dataset is held, and exactly one incident opens. It survives a green rerun of
// the same job, closes on the release and not before, and the next hold on the
// same dataset opens a second incident. Its evidence names the producer's
// tenant, and a downstream node of another tenant only as a placeholder.
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
	s.runProducer(producer)
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
	s.NotContains(first.AllowedJobs, foreignConsumer, "the cone is evidence, not authorization")

	var evidence holdIncidentEvidence
	s.Require().NoError(json.Unmarshal(first.Evidence, &evidence), string(first.Evidence))
	s.Equal("", evidence.DatasetNamespace, "the dataset's namespace is a data axis, not the tenant")
	s.Equal(dataset, evidence.Dataset)
	s.Require().NotEmpty(evidence.HoldID)
	s.Require().Len(evidence.Violations, 1)
	s.Equal("min", evidence.Violations[0].Assertion)
	s.Require().NotNil(evidence.Violations[0].Observed)
	s.InDelta(12, *evidence.Violations[0].Observed, 0.001)
	s.Require().NotNil(evidence.Violations[0].Bound)
	s.InDelta(1000, *evidence.Violations[0].Bound, 0.001)

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
