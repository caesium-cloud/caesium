//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Data-circuit-breaker F5: `caesium why` answers "why did nothing run?" for a
// run the hold gate refused, and "why did this run on bad data?" for one that
// was admitted past a hold. These scenarios drive the real CLI and endpoint
// against real holds opened by real producer runs.

// whyHoldExplanation mirrors the fields of `caesium why --json` these scenarios
// read.
type whyHoldExplanation struct {
	Verdict    string `json:"verdict"`
	Status     string `json:"status"`
	SkipReason string `json:"skipReason"`
	Summary    string `json:"summary"`
	Trigger    struct {
		Hold *struct {
			Disposition string `json:"disposition"`
			Holds       []struct {
				HoldID         string `json:"holdId"`
				Dataset        string `json:"dataset"`
				HeldSinceRunID string `json:"heldSinceRunId"`
				Detail         string `json:"detail"`
				Violations     []struct {
					Metric    string   `json:"metric"`
					Assertion string   `json:"assertion"`
					Observed  *float64 `json:"observed"`
					Bound     *float64 `json:"bound"`
				} `json:"violations"`
			} `json:"holds"`
		} `json:"hold"`
	} `json:"trigger"`
}

// whyStdout runs `caesium why` capturing stdout apart from stderr and returns
// stdout. A merged capture would hide log lines leaking onto it.
func (s *IntegrationTestSuite) whyStdout(jobID, runID, task string, extra ...string) string {
	s.T().Helper()
	args := append([]string{"why", runID, "--job-id", jobID, "--task", task, "--server", s.caesiumURL}, extra...)
	out, err := s.runCLIStdout(args...)
	s.Require().NoError(err, "caesium why failed:\n%s", out)
	return out
}

// whyJSON runs `caesium why --json` and requires clean, parseable stdout.
func (s *IntegrationTestSuite) whyJSON(jobID, runID, task string) whyHoldExplanation {
	s.T().Helper()
	out := s.whyStdout(jobID, runID, task, "--json")
	s.Require().True(json.Valid([]byte(out)), "caesium why --json stdout was not clean JSON:\n%s", out)
	var exp whyHoldExplanation
	s.Require().NoError(json.Unmarshal([]byte(out), &exp))
	return exp
}

func firstLine(out string) string {
	line, _, _ := strings.Cut(out, "\n")
	return line
}

// activeHoldID reads the id of the one active hold on dataset through the
// operator endpoint.
func (s *IntegrationTestSuite) activeHoldID(dataset string) string {
	s.T().Helper()
	var holds struct {
		Holds []struct {
			ID string `json:"id"`
		} `json:"holds"`
	}
	s.getJSON("/v1/datasets/holds?name="+url.QueryEscape(dataset), &holds)
	s.Require().Len(holds.Holds, 1, "dataset %s must have exactly one active hold", dataset)
	return holds.Holds[0].ID
}

// TestWhyExplainsHoldSkippedRun is the F5 headline. A producer breaches its
// min and holds the dataset; its consumer's run is refused; `caesium why` on
// that run names the dataset, the assertion, the observed and bound values and
// the hold. The answer is fixed when the run is skipped: a repeat breach
// replaces the hold's verdicts and the holder's clean run releases it, and the
// older run still says what it said.
func (s *IntegrationTestSuite) TestWhyExplainsHoldSkippedRun() {
	s.requireDataAssertionsLane()

	suffix := time.Now().UnixNano()
	producerAlias := fmt.Sprintf("integration-why-hold-producer-%d", suffix)
	consumerAlias := fmt.Sprintf("integration-why-hold-consumer-%d", suffix)
	dataset := fmt.Sprintf("integration.why.hold.%d", suffix)
	// The consumer goes first: a producer deleted under a live consumer would
	// fail every later apply's dataset-graph check.
	s.cleanupInOrder(consumerAlias, producerAlias)

	producer := s.applyHoldProducer(producerAlias, dataset, "auto", 12)
	opener := s.runProducer(producer)
	consumer := s.applyAssertionsJob(consumerAlias, holdConsumerManifest(consumerAlias, dataset, ""))
	held := s.triggerHeldRun(consumer.ID)
	holdID := s.holdIDFromSkippedRun(held)

	want := fmt.Sprintf("SKIPPED — dataset /%s held since run %s (assertion rowCount.min: observed 12 vs bound 1000; hold %s)",
		dataset, opener.ID, holdID)

	out := s.whyStdout(consumer.ID, held.ID, "consume")
	s.Equal(want, firstLine(out), "the summary is the first line of the table")
	s.Contains(out, "SKIPPED")
	s.Contains(out, "Dataset holds (skipped, 1):")
	s.Contains(out, "rowCount.min observed 12 vs bound 1000")

	exp := s.whyJSON(consumer.ID, held.ID, "consume")
	s.Equal("SKIPPED", exp.Verdict)
	s.Equal("skipped", exp.Status)
	s.Equal(want, exp.Summary)
	s.Contains(exp.SkipReason, "dataset_hold:/"+dataset)
	s.Require().NotNil(exp.Trigger.Hold)
	s.Equal("skipped", exp.Trigger.Hold.Disposition)
	s.Require().Len(exp.Trigger.Hold.Holds, 1)
	got := exp.Trigger.Hold.Holds[0]
	s.Equal(holdID, got.HoldID)
	s.Equal(dataset, got.Dataset)
	s.Equal(opener.ID, got.HeldSinceRunID)
	s.Equal("recorded", got.Detail)
	s.Require().Len(got.Violations, 1)
	s.Require().NotNil(got.Violations[0].Observed)
	s.InDelta(12, *got.Violations[0].Observed, 0.001)
	s.Require().NotNil(got.Violations[0].Bound)
	s.InDelta(1000, *got.Violations[0].Bound, 0.001)

	// The endpoint serves the same answer the CLI renders.
	var rest whyHoldExplanation
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s/why?task=consume", consumer.ID, held.ID), &rest)
	s.Equal(want, rest.Summary)

	// A repeat breach folds into the same hold and replaces its verdicts. A run
	// skipped after it carries the new ones; the first run is unchanged.
	producer = s.applyHoldProducer(producerAlias, dataset, "auto", 7)
	s.runProducer(producer)
	heldAgain := s.triggerHeldRun(consumer.ID)
	s.Equal(
		fmt.Sprintf("SKIPPED — dataset /%s held since run %s (assertion rowCount.min: observed 7 vs bound 1000; hold %s)",
			dataset, opener.ID, holdID),
		firstLine(s.whyStdout(consumer.ID, heldAgain.ID, "consume")),
		"a run skipped after the repeat breach carries the verdicts the hold then held")
	s.Equal(want, firstLine(s.whyStdout(consumer.ID, held.ID, "consume")),
		"a repeat breach must not rewrite why an earlier run was skipped")

	// The holder's next clean run releases the hold (release: auto).
	producer = s.applyHoldProducer(producerAlias, dataset, "auto", 5000)
	s.runProducer(producer)
	reopenedID := s.triggerRun(consumer.ID)
	s.Equal("succeeded", s.awaitRun(consumer.ID, reopenedID, runTimeout).Status, "the release must have reopened the gate")
	s.Equal(want, firstLine(s.whyStdout(consumer.ID, held.ID, "consume")),
		"a release must not rewrite why an earlier run was skipped")
	s.Equal(want, s.whyJSON(consumer.ID, held.ID, "consume").Summary)
}

// TestWhyExplainsSeveralHeldDatasetsAndAdmissionPastAHold covers the other two
// shapes a hold takes in a run: a consumer blocked by more than one held
// dataset (the summary names the oldest in full and the rest by name, the JSON
// lists every hold), and a consumer that runs on held data because it declares
// metadata.onUpstreamHold: run.
func (s *IntegrationTestSuite) TestWhyExplainsSeveralHeldDatasetsAndAdmissionPastAHold() {
	s.requireDataAssertionsLane()

	suffix := time.Now().UnixNano()
	first := fmt.Sprintf("integration.why.multi.a.%d", suffix)
	second := fmt.Sprintf("integration.why.multi.b.%d", suffix)
	firstAlias := fmt.Sprintf("integration-why-multi-a-%d", suffix)
	secondAlias := fmt.Sprintf("integration-why-multi-b-%d", suffix)
	consumerAlias := fmt.Sprintf("integration-why-multi-consumer-%d", suffix)
	optOutAlias := fmt.Sprintf("integration-why-optout-consumer-%d", suffix)
	// Every consumer before the producers it reads.
	s.cleanupInOrder(optOutAlias, consumerAlias, firstAlias, secondAlias)

	firstProducer := s.applyHoldProducer(firstAlias, first, "auto", 12)
	firstRun := s.runProducer(firstProducer)
	secondProducer := s.applyHoldProducer(secondAlias, second, "auto", 3)
	s.runProducer(secondProducer)
	firstHold, secondHold := s.activeHoldID(first), s.activeHoldID(second)

	manifest := strings.Replace(holdConsumerManifest(consumerAlias, first, ""),
		fmt.Sprintf("        - %s\n", first), fmt.Sprintf("        - %s\n        - %s\n", first, second), 1)
	s.Require().Contains(manifest, second, "the consumer must declare both datasets")
	consumer := s.applyAssertionsJob(consumerAlias, manifest)
	held := s.triggerHeldRun(consumer.ID)

	wantFirst := fmt.Sprintf("SKIPPED — dataset /%s held since run %s (assertion rowCount.min: observed 12 vs bound 1000; hold %s); also held: /%s (hold %s)",
		first, firstRun.ID, firstHold, second, secondHold)
	s.Equal(wantFirst, firstLine(s.whyStdout(consumer.ID, held.ID, "consume")))
	exp := s.whyJSON(consumer.ID, held.ID, "consume")
	s.Require().NotNil(exp.Trigger.Hold)
	s.Require().Len(exp.Trigger.Hold.Holds, 2, "every hold stays in the JSON")
	s.Equal(firstHold, exp.Trigger.Hold.Holds[0].HoldID)
	s.Equal(secondHold, exp.Trigger.Hold.Holds[1].HoldID)
	s.Equal(second, exp.Trigger.Hold.Holds[1].Dataset)

	// An opted-out consumer runs, and `why` says it ran on held data.
	optOut := s.applyAssertionsJob(optOutAlias, holdConsumerManifest(optOutAlias, first, "run"))
	runID := s.triggerRun(optOut.ID)
	s.Equal("succeeded", s.awaitRun(optOut.ID, runID, runTimeout).Status)
	admitted := s.whyJSON(optOut.ID, runID, "consume")
	s.NotEqual("SKIPPED", admitted.Verdict, "the task ran")
	s.Contains(admitted.Summary, fmt.Sprintf(
		"admitted by metadata.onUpstreamHold: run on dataset /%s held since run %s (assertion rowCount.min: observed 12 vs bound 1000; hold %s)",
		first, firstRun.ID, firstHold))
	s.Require().NotNil(admitted.Trigger.Hold)
	s.Equal("admitted", admitted.Trigger.Hold.Disposition)
}

// TestWhyExplainsTriggerRuleSkip pins the verdict for the skips that have
// nothing to do with a hold: a step the failure of its predecessor resolved
// without running reads SKIPPED with the reason the row recorded, where it used
// to read UNKNOWN.
func (s *IntegrationTestSuite) TestWhyExplainsTriggerRuleSkip() {
	alias := fmt.Sprintf("integration-why-skip-%d", time.Now().UnixNano())
	s.cleanupInOrder(alias)
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
  cache: false
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: head
    image: alpine:3.23
    command: ["sh","-c","exit 1"]
    next: [tail]
  - name: tail
    image: alpine:3.23
    command: ["sh","-c","echo never"]
`, alias)
	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Equal("failed", run.Status)
	tail := s.requireRunTaskByName(job.ID, run, "tail")
	s.Require().Equal("skipped", tail.Status)
	s.Require().NotEmpty(tail.Error, "a skipped step records why")

	exp := s.whyJSON(job.ID, runID, "tail")
	s.Equal("SKIPPED", exp.Verdict)
	s.Equal(tail.Error, exp.SkipReason)
	s.Equal("SKIPPED — "+tail.Error, exp.Summary)
	s.Nil(exp.Trigger.Hold, "no hold touched this run")
	s.Equal("SKIPPED — "+tail.Error, firstLine(s.whyStdout(job.ID, runID, "tail")))
}
