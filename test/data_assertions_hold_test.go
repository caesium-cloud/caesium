//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

// holdProducerManifest is a one-step job that emits ##caesium::metrics and
// declares a rowCount floor under onViolation: hold. `cache: false` is
// load-bearing: the default lane runs with CAESIUM_CACHE_ENABLED=true, and a
// cached task emits no new sample, so the second trigger of an unchanged
// manifest would observe nothing at all.
func holdProducerManifest(alias, dataset, step, release string) string {
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
          assertions:
            rowCount: {min: 1000}
          onViolation: hold
          release: %s
`, alias, step, dataset, release)
}

// holdConsumerManifest is a downstream job that declares it consumes `dataset`.
// onUpstreamHold is written verbatim into metadata (pass "" for the default).
func holdConsumerManifest(alias, dataset, onUpstreamHold string) string {
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
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: consume
    image: %s
    command: ["sh","-c","echo consuming"]
    datasets:
      consumes:
        - %s
`, alias, policy, metricsFixtureImage, dataset)
}

// applyHoldProducer applies the producer at a given emitted rowCount and
// returns the job. Re-applying the same alias UPDATES the job in place (the
// importer preserves the row's id), which is what lets a scenario replace a
// breaching run with a clean one and still have the SAME job hold the dataset.
func (s *IntegrationTestSuite) applyHoldProducer(alias, dataset, release string, rowCount float64) *jobSummary {
	s.T().Helper()

	step, err := metricsProducerStepForDataset("load", dataset, map[string]any{"rowCount": rowCount})
	s.Require().NoError(err)
	return s.applyAssertionsJob(alias, holdProducerManifest(alias, dataset, step, release))
}

// runProducer triggers the producer and requires the run to SUCCEED. The hold
// disposition never reddens a run: the work is done, it is the data that is
// wrong.
func (s *IntegrationTestSuite) runProducer(job *jobSummary) *runResponse {
	s.T().Helper()

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Require().Equal("succeeded", run.Status,
		"onViolation: hold must let the producing task succeed — the DATASET is what breaks")
	return run
}

// triggerHeldRun starts a run the admission gate is expected to refuse and
// returns the terminal `skipped` run it recorded.
//
// The POST answers 202 with no body (the gate returns ErrRunSkipped, so the
// controller has no run to hand back), which is exactly why the run has to be
// findable in history: an invisible non-run would be silent poison's evil twin.
func (s *IntegrationTestSuite) triggerHeldRun(jobID string) runResponse {
	s.T().Helper()

	before := len(s.fetchRuns(jobID))
	resp, err := s.doJSONRequest(http.MethodPost, fmt.Sprintf("%v/v1/jobs/%s/run", s.caesiumURL, jobID), nil)
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Require().Equal(http.StatusAccepted, resp.StatusCode)

	var latestID string
	s.Require().Eventually(func() bool {
		runs := s.fetchRuns(jobID)
		if len(runs) <= before {
			return false
		}
		latest := runs[len(runs)-1]
		latestID = latest.ID
		return latest.Status != "running"
	}, 60*time.Second, 250*time.Millisecond,
		"a hold-gated run must appear in run history as a terminal row, not as an absence")

	// Re-read the run by id: the run LIST projection carries no task rows, and
	// the whole point of this path is that the skipped run has them.
	return s.fetchRun(jobID, latestID)
}

// holdIDFromSkippedRun extracts the hold id from a skipped task row's error
// text ("dataset_hold:<ns>/<name> hold=<uuid>").
//
// It is the only REST-visible route to a hold id until Stream D ships
// GET /v1/datasets/holds, and it doubles as an assertion that the reason text
// really carries the hold — a skipped task that named no hold would leave an
// operator with nothing to release.
func (s *IntegrationTestSuite) holdIDFromSkippedRun(run runResponse) string {
	s.T().Helper()

	pattern := regexp.MustCompile(`hold=([0-9a-fA-F-]{36})`)
	for _, task := range run.Tasks {
		if match := pattern.FindStringSubmatch(task.Error); match != nil {
			return match[1]
		}
	}
	s.T().Fatalf("no skipped task named its hold; run %s tasks: %+v", run.ID, run.Tasks)
	return ""
}

// TestDataAssertionsHoldGatesDownstreamAndAutoReleases is Stream C end to end
// on the real surface: a breach opens a hold on a green run, the downstream
// consumer is admitted straight to `skipped` with a reason and skipped task
// rows, and the holder's next clean run reopens the circuit.
func (s *IntegrationTestSuite) TestDataAssertionsHoldGatesDownstreamAndAutoReleases() {
	s.requireDataAssertionsLane()

	suffix := time.Now().UnixNano()
	producerAlias := fmt.Sprintf("integration-hold-producer-%d", suffix)
	consumerAlias := fmt.Sprintf("integration-hold-consumer-%d", suffix)
	dataset := fmt.Sprintf("integration.hold.%d", suffix)

	// 1. A breaching producer run. It SUCCEEDS and holds the dataset.
	producer := s.applyHoldProducer(producerAlias, dataset, "auto", 12)
	run := s.runProducer(producer)
	task := s.requireRunTaskByName(producer.ID, run, "load")
	s.Equal("succeeded", task.Status)
	s.Require().NotEmpty(task.DataViolations, "the breach is recorded on the green run")
	s.Equal("min", task.DataViolations[0].Assertion)

	// 2. The downstream consumer is gated.
	consumer := s.applyAssertionsJob(consumerAlias, holdConsumerManifest(consumerAlias, dataset, ""))
	held := s.triggerHeldRun(consumer.ID)
	s.Equal("skipped", held.Status, "a run that consumes a held dataset must not execute")
	s.Equal("dataset_hold:/"+dataset, held.SkipReason,
		"run history must say WHY nothing ran")
	s.Require().NotEmpty(held.Tasks, "a hold-skipped run has task rows; 'a run with no tasks' is not a readable shape")
	for _, row := range held.Tasks {
		s.Equal("skipped", row.Status)
		s.Contains(row.Error, "dataset_hold:/"+dataset)
		s.Contains(row.Error, "hold=")
	}
	s.NotEmpty(s.holdIDFromSkippedRun(held))

	// 3. The holder's next CLEAN run releases the hold (release: auto).
	producer = s.applyHoldProducer(producerAlias, dataset, "auto", 5000)
	clean := s.runProducer(producer)
	cleanTask := s.requireRunTaskByName(producer.ID, clean, "load")
	s.Empty(cleanTask.DataViolations, "the rerun must be genuinely clean for the release to mean anything")

	// 4. The gate is open again.
	reopenedID := s.triggerRun(consumer.ID)
	reopened := s.awaitRun(consumer.ID, reopenedID, runTimeout)
	s.Equal("succeeded", reopened.Status, "a released dataset admits its consumers again")
	s.Empty(reopened.SkipReason)
}

// TestDataAssertionsOnUpstreamHoldRunIgnoresTheGate proves the per-job opt-out
// is honoured end to end — metadata.onUpstreamHold: run persists onto the job
// row and the admission gate reads it there.
func (s *IntegrationTestSuite) TestDataAssertionsOnUpstreamHoldRunIgnoresTheGate() {
	s.requireDataAssertionsLane()

	suffix := time.Now().UnixNano()
	producerAlias := fmt.Sprintf("integration-hold-optout-producer-%d", suffix)
	consumerAlias := fmt.Sprintf("integration-hold-optout-consumer-%d", suffix)
	dataset := fmt.Sprintf("integration.hold.optout.%d", suffix)

	producer := s.applyHoldProducer(producerAlias, dataset, "auto", 12)
	s.runProducer(producer)

	consumer := s.applyAssertionsJob(consumerAlias, holdConsumerManifest(consumerAlias, dataset, "run"))
	runID := s.triggerRun(consumer.ID)
	run := s.awaitRun(consumer.ID, runID, runTimeout)
	s.Equal("succeeded", run.Status,
		"metadata.onUpstreamHold: run opts a job out of the gate: it runs on held data anyway")
	s.Empty(run.SkipReason)
}

// TestDataAssertionsManualReleaseSurvivesACleanRun is the other half of A3's
// `release` field: declared `manual`, the hold outlives a clean producer run
// and keeps gating consumers until a human acks it.
func (s *IntegrationTestSuite) TestDataAssertionsManualReleaseSurvivesACleanRun() {
	s.requireDataAssertionsLane()

	suffix := time.Now().UnixNano()
	producerAlias := fmt.Sprintf("integration-hold-manual-producer-%d", suffix)
	consumerAlias := fmt.Sprintf("integration-hold-manual-consumer-%d", suffix)
	dataset := fmt.Sprintf("integration.hold.manual.%d", suffix)

	producer := s.applyHoldProducer(producerAlias, dataset, "manual", 12)
	s.runProducer(producer)

	consumer := s.applyAssertionsJob(consumerAlias, holdConsumerManifest(consumerAlias, dataset, ""))
	s.Equal("skipped", s.triggerHeldRun(consumer.ID).Status)

	// A clean run of the holder — which under `release: auto` would clear it.
	producer = s.applyHoldProducer(producerAlias, dataset, "manual", 5000)
	clean := s.runProducer(producer)
	s.Empty(s.requireRunTaskByName(producer.ID, clean, "load").DataViolations)

	still := s.triggerHeldRun(consumer.ID)
	s.Equal("skipped", still.Status,
		"release: manual must NOT auto-release; the hold waits for a human ack")
	s.Equal("dataset_hold:/"+dataset, still.SkipReason)
}

// TestDataAssertionsRepeatBreachAppendsAnOccurrence pins alert-once on the live
// server: a second breach of an already-held dataset increments the occurrence
// counter and emits NO second dataset_held event.
//
// It reads dataset_holds and execution_events through the shared catalog handle
// rather than a route, because GET /v1/datasets/holds belongs to Stream D (W4)
// and does not exist yet — the same deliberate, temporary deviation W1's
// metrics scenario records.
func (s *IntegrationTestSuite) TestDataAssertionsRepeatBreachAppendsAnOccurrence() {
	s.requireDataAssertionsLane()

	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("integration-hold-repeat-%d", suffix)
	dataset := fmt.Sprintf("integration.hold.repeat.%d", suffix)

	producer := s.applyHoldProducer(alias, dataset, "auto", 12)
	s.runProducer(producer)
	s.runProducer(producer)

	conn := s.openIntegrationCatalogGorm()

	var holds []models.DatasetHold
	s.Require().Eventually(func() bool {
		holds = nil
		if err := conn.Where("name = ?", dataset).Find(&holds).Error; err != nil {
			return false
		}
		return len(holds) == 1 && holds[0].OccurrenceCount >= 2
	}, 60*time.Second, 500*time.Millisecond,
		"two breaches must fold into ONE active hold with two occurrences")

	s.Equal(models.DatasetHoldStatusActive, holds[0].Status)
	s.Equal(2, holds[0].OccurrenceCount)
	s.Equal("min", holds[0].Reason)
	s.Contains(string(holds[0].Violations), `"assertion":"min"`,
		"the hold carries the verdict, baseline snapshot included, for the agent bundle")

	var alerts int64
	s.Require().NoError(conn.Model(&models.ExecutionEvent{}).
		Where("type = ? AND job_id = ?", "dataset_held", producer.ID).
		Count(&alerts).Error)
	s.Equal(int64(1), alerts,
		"alert-once is structural: an occurrence append must not re-emit dataset_held")
}

// TestDataAssertionsHoldReleaseRefusedWithoutAuth is the fail-closed contract on
// the default (AUTH_MODE=none) lane: the release endpoint exists — the feature
// flag is on — but it refuses with a 403 that NAMES the precondition, because a
// release must record an authenticated principal and cannot when every caller
// is anonymous.
func (s *IntegrationTestSuite) TestDataAssertionsHoldReleaseRefusedWithoutAuth() {
	s.requireDataAssertionsLane()
	s.skipOnAuthLane("the auth lane runs the api-key success path instead")

	target := fmt.Sprintf("%v/v1/datasets/holds/%s/release", s.caesiumURL, uuid.NewString())
	body, err := json.Marshal(map[string]string{"reason": "manual override"})
	s.Require().NoError(err)

	resp, err := s.doJSONRequest(http.MethodPost, target, bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close()

	payload := make(map[string]any)
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&payload))
	s.Require().Equal(http.StatusForbidden, resp.StatusCode,
		"an anonymous release must be refused, not silently attributed to nobody")
	s.Contains(fmt.Sprint(payload), "CAESIUM_AUTH_MODE",
		"the refusal must name the precondition an operator has to change")
}

// TestHoldReleaseWithAPIKeyReopensTheGate is the auth lane's half: an
// authenticated operator acks a real hold through the real route, and the
// downstream consumer is admitted again.
func (s *IntegrationTestSuite) TestHoldReleaseWithAPIKeyReopensTheGate() {
	s.requireAuthLane()
	s.requireDataAssertionsLane()

	suffix := time.Now().UnixNano()
	producerAlias := fmt.Sprintf("integration-hold-ack-producer-%d", suffix)
	consumerAlias := fmt.Sprintf("integration-hold-ack-consumer-%d", suffix)
	dataset := fmt.Sprintf("integration.hold.ack.%d", suffix)

	producer := s.applyHoldProducer(producerAlias, dataset, "manual", 12)
	s.runProducer(producer)

	consumer := s.applyAssertionsJob(consumerAlias, holdConsumerManifest(consumerAlias, dataset, ""))
	held := s.triggerHeldRun(consumer.ID)
	s.Require().Equal("skipped", held.Status)
	holdID := s.holdIDFromSkippedRun(held)

	target := fmt.Sprintf("%v/v1/datasets/holds/%s/release", s.caesiumURL, holdID)
	body, err := json.Marshal(map[string]any{
		"reason":   "source backfilled",
		"tolerate": map[string]string{"rowCount": "24h"},
	})
	s.Require().NoError(err)

	resp, err := s.doJSONRequest(http.MethodPost, target, bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close()

	var released struct {
		Hold struct {
			ID            string `json:"id"`
			Status        string `json:"status"`
			ReleaseReason string `json:"release_reason"`
			ReleasedBy    string `json:"released_by"`
		} `json:"hold"`
	}
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&released))
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	s.Equal(holdID, released.Hold.ID)
	s.Equal("released", released.Hold.Status)
	s.Equal("manual_ack", released.Hold.ReleaseReason)
	s.NotEmpty(released.Hold.ReleasedBy, "the release always records an authenticated principal")

	// The gate reopened.
	runID := s.triggerRun(consumer.ID)
	run := s.awaitRun(consumer.ID, runID, runTimeout)
	s.Equal("succeeded", run.Status, "an acked hold admits the consumer again")

	// A second ack on the same hold is a conflict, not a silent re-release.
	again, err := s.doJSONRequest(http.MethodPost, target, bytes.NewReader(body))
	s.Require().NoError(err)
	defer again.Body.Close()
	s.Equal(http.StatusConflict, again.StatusCode)
}
