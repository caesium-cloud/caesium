//go:build integration

package test

import (
	"fmt"
	"strings"
	"time"
)

// data_assertions_capture_test.go drives issue #437 through the real surface: a
// step whose ##caesium::metrics stream overflows the 16 KiB marker cap loses
// the very metric its contract declares, and that loss must NOT read as a
// broken contract.
//
// Before the fix, the dropped metric produced a `missing` violation, which
// under `onViolation: fail` turned a perfectly good run red for an
// infrastructure reason. The scenario below asserts the opposite outcome on a
// live server: the run succeeds, and the recorded verdict is the warn-only
// `unavailable` kind naming the truncated marker stream.

// truncatingMetricsFillers is the number of filler metrics the fixture step
// emits before its declared one. Each distinct (dataset, metric) pair costs
// len(dataset) + len(metric) + 32 bytes against pkgtask.MaxMetricsBytes
// (16 KiB), so with the ~45-character dataset names this file generates, fewer
// than 200 entries already fill the cap — 600 leaves the margin that keeps this
// scenario from silently going vacuous if those constants move.
const truncatingMetricsFillers = 600

// truncatingMetricsStep returns a `steps:` list entry whose container floods
// the metrics accumulator with filler samples and only then emits the declared
// `rowCount`, which the cap therefore drops. The shell loop concatenates
// single-quoted JSON around the bare counter, so no marker line needs a
// double-quote the surrounding YAML scalar would have to escape twice.
func truncatingMetricsStep(stepName, dataset string) string {
	script := fmt.Sprintf(
		`i=0; while [ $i -lt %d ]; do echo '##caesium::metrics {"dataset":"%s","filler'$i'":1}'; i=$((i+1)); done; `+
			`echo '##caesium::metrics {"dataset":"%s","rowCount":5000}'`,
		truncatingMetricsFillers, dataset, dataset)

	return fmt.Sprintf(`  - name: %s
    image: %s
    command: ["sh","-c","%s"]
`, stepName, metricsFixtureImage, strings.ReplaceAll(script, `"`, `\"`))
}

// TestDataAssertionsTruncatedMarkerStreamIsUnavailableNotMissing is the
// end-to-end regression for #437. The declared contract is `onViolation: fail`
// on purpose: that is the mode the bug reddened.
func (s *IntegrationTestSuite) TestDataAssertionsTruncatedMarkerStreamIsUnavailableNotMissing() {
	s.requireDataAssertionsLane()

	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("integration-assert-truncated-%d", suffix)
	dataset := fmt.Sprintf("integration.assert.truncated.%d", suffix)

	job := s.applyAssertionsJob(alias, assertionsEvaluatorManifest(alias, dataset,
		truncatingMetricsStep("load", dataset),
		"            rowCount: {min: 1000}", "fail"))

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Require().Equal("succeeded", run.Status,
		"a marker stream the executor could not read in full is an infrastructure fault; "+
			"it must not fail a run declared onViolation: fail")

	task := s.requireRunTaskByName(job.ID, run, "load")
	s.Equal("succeeded", task.Status)
	s.Empty(task.Error)

	s.Require().NotEmpty(task.DataViolations,
		"the lost observation must still be recorded and surfaced, not silently dropped")
	violation := task.DataViolations[0]
	s.Equal(dataset, violation.Dataset)
	s.Equal("rowCount", violation.Metric)
	s.Equal("unavailable", violation.Assertion,
		"a metric dropped by the marker cap is unavailable, never missing — the step did emit it")
	s.Equal("marker_stream_truncated", violation.Reason,
		"the operator must be able to tell WHICH way the observation was lost")
	s.Nil(violation.Observed, "nothing was observed for this metric")
	s.Contains(violation.Message, "##caesium::metrics")
}
