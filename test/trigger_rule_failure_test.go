//go:build integration

package test

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// A FAILED PLAIN TASK must release its successors: the tolerant ones
// (`all_done`) reach outstanding_predecessors = 0 and are announced ready, the
// intolerant ones (`all_success`) are skipped with the rule reason. The SQL
// lane did neither — resolveInstanceFailureTx returned early for any row that
// was not a fan-out instance, so the row scalar every dispatcher gates on was
// never decremented for a plain failure and every consumer downstream of one
// was stranded:
//
//   - a FANNED consumer even in local mode (runFannedGroup reads that scalar
//     off the row, so its instances swept as "fan-out instance was never
//     dispatched (unresolved in-group dependency)"),
//   - EVERY consumer in distributed mode (ClaimNext, ClaimTaskForDispatch and
//     PendingTasksForDispatch all require outstanding_predecessors = 0),
//   - and an `all_success` consumer sat `pending` on a TERMINAL run instead of
//     being skipped, so "held by its trigger rule" and "never dispatched" were
//     indistinguishable — the distinction Plan 1's admission gate is built on.
//
// WHAT THESE SCENARIOS DELIBERATELY DO NOT ASSERT, and why. Every integration
// lane runs with CAESIUM_TASK_FAILURE_POLICY=halt (pkg/env's default; no
// `integration-up*` recipe overrides it), and `halt` means what it says on both
// lanes: the local Kahn loop clears its queue the moment any task fails
// (internal/job/job.go, `if !continueOnFailure { halt = true }`) and the
// distributed waiter finalizes the run as soon as a task has failed and nothing
// is running (waitForRunCompletion, "run %s halted after %d failed task(s)"),
// after which ClaimNext's `jr.status = running` predicate refuses the row. So
// no step downstream of a failure EXECUTES on any lane as configured, and a
// scenario asserting that the tolerant consumer ran would be asserting the
// failure policy — or racing it — rather than A1. What these assert is exactly
// what A1 changes and what all three dispatch paths then read: the successors
// are RESOLVED — released to zero, or skipped with their rule reason — instead
// of left pending forever on a terminal run.
const triggerRuleFailureRunTimeout = 3 * time.Minute

// plainFailureRun is a narrow view of GET /v1/jobs/:id/runs/:run_id. It is
// declared here rather than reusing runResponse because the assertion needs
// `outstanding_predecessors`, the scalar the whole bug is about, and a
// fan-out group's entry in this payload is the COLLAPSED group (the run detail
// folds instances by catalog task id, so `id` here is the catalog task id and
// the group carries its first instance's outstanding count).
type plainFailureRun struct {
	ID     string             `json:"id"`
	Status string             `json:"status"`
	Tasks  []plainFailureTask `json:"tasks"`
}

type plainFailureTask struct {
	ID                      string `json:"id"`
	Status                  string `json:"status"`
	Error                   string `json:"error,omitempty"`
	OutstandingPredecessors int    `json:"outstanding_predecessors"`
}

// ownerInMemoryLane reports whether this suite run drives a server that
// resolves completions through internal/run/owner_state.go
// RunState.ApplyCompletion (`just integration-test-owner-memory`) rather than
// through the SQL store. That path was already correct for a plain failure —
// which is why A2 is a green-before/green-after regression guard there — but it
// advances the DAG IN MEMORY and deliberately does not decrement
// outstanding_predecessors in SQL (see TestCompleteTaskOwner's "owner path must
// not decrement successors in SQL"), so the row-scalar assertion is SQL-lane
// only. The rule-skip assertion holds on every lane: owner_state.go emits the
// byte-identical reason.
func ownerInMemoryLane() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("CAESIUM_RUN_OWNER_IN_MEMORY")), "true")
}

// plainFailureManifest renders the DAG both scenarios drive:
//
//	list ──▶ gate (exits 1) ──┬──▶ tolerant (triggerRule: all_done)
//	  │                       └──▶ strict   (triggerRule: all_success)
//	  └───────────────────────────▶ tolerant
//
// `list` runs first and succeeds, so when `fanned` is true the tolerant
// consumer is materialized from its partition list BEFORE `gate` fails and
// every instance is provably waiting on `gate` at the moment of the failure.
func plainFailureManifest(alias string, fanned bool) string {
	fanOut := ""
	consumerCmd := "echo tolerant ran"
	if fanned {
		consumerCmd = "echo tolerant ran for $CAESIUM_PARTITION"
		fanOut = `    fanOut:
      from: list
      maxPartitions: 8
`
	}
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: list
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::partitions [\"p0\",\"p1\"]'"]
    next: [gate, tolerant]
  - name: gate
    image: alpine:3.23
    command: ["sh", "-c", "echo gate failing; exit 1"]
    dependsOn: [list]
    next: [tolerant, strict]
  - name: tolerant
    image: alpine:3.23
    command: ["sh", "-c", %q]
    dependsOn: [list, gate]
    triggerRule: all_done
%s  - name: strict
    image: alpine:3.23
    command: ["sh", "-c", "echo strict ran"]
    dependsOn: [gate]
    triggerRule: all_success
`, alias, consumerCmd, fanOut)
}

// assertPlainFailureResolvedSuccessors is the contract every lane shares.
func (s *IntegrationTestSuite) assertPlainFailureResolvedSuccessors(jobID, runID string) {
	s.T().Helper()

	var run plainFailureRun
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s", jobID, runID), &run)

	byName := map[string]plainFailureTask{}
	for _, name := range []string{"list", "gate", "tolerant", "strict"} {
		id := s.jobTaskIDByName(jobID, name)
		for _, t := range run.Tasks {
			if t.ID == id {
				byName[name] = t
			}
		}
	}
	s.Require().Len(byName, 4, "every step must have a run task: %+v", run.Tasks)
	s.Require().Equal("failed", byName["gate"].Status, "the fixture must actually fail `gate`: %+v", byName)

	// The intolerant consumer is RESOLVED by its rule, not stranded pending on
	// a terminal run.
	strict := byName["strict"]
	s.Equal("skipped", strict.Status,
		"an all_success consumer of a failed plain task must be skipped, not left pending: %+v", byName)
	s.Equal(`trigger rule "all_success" not satisfied`, strict.Error,
		"the skip must carry the byte-identical trigger-rule reason every other advancement path emits")

	// The tolerant consumer is RELEASED: the scalar the local runFannedGroup and
	// every distributed claim predicate read is zero.
	if ownerInMemoryLane() {
		return
	}
	tolerant := byName["tolerant"]
	s.Zero(tolerant.OutstandingPredecessors,
		"an all_done consumer of a FAILED plain task must be released (outstanding_predecessors=0), not stranded: %+v", tolerant)
	s.NotEqual("skipped", tolerant.Status,
		"all_done is satisfied by a failed predecessor; the consumer must not be skipped: %+v", tolerant)
}

// TestPlainFailureReleasesAllDoneFannedConsumer is the local lane's half: a
// FANNED tolerant consumer, which local mode strands even though its in-memory
// Kahn loop knows better — runFannedGroup reads each instance's
// outstanding_predecessors from the row, so a group whose scalar never advanced
// sweeps as "never dispatched".
func (s *IntegrationTestSuite) TestPlainFailureReleasesAllDoneFannedConsumer() {
	alias := fmt.Sprintf("plain-failure-fanned-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(plainFailureManifest(alias, true))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	tolerantID := s.jobTaskIDByName(job.ID, "tolerant")

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, triggerRuleFailureRunTimeout)
	s.Equal("failed", run.Status, "a failed plain step must fail the run")

	s.assertPlainFailureResolvedSuccessors(job.ID, runID)

	parts := s.expandedPartitions(s.listPartitions(job.ID, runID, tolerantID))
	s.Require().Len(parts, 2, "both partitions must materialize: %v", partitionStatusMap(parts))
	for _, p := range parts {
		s.NotContains(p.Error, "never dispatched",
			"partition %q must not be swept as stranded once the failed predecessor releases it", p.Value)
	}
}

// TestPlainFailureReleasesAllDoneConsumerDistributed is the distributed lane's
// half, and the one that shows why this is a P0: the distributed claimer has no
// in-memory DAG at all, so an unadvanced outstanding_predecessors leaves even a
// PLAIN tolerant consumer unclaimable for the life of the run.
//
// On `-owner-memory` the same run resolves through
// internal/run/owner_state.go RunState.ApplyCompletion, which was already
// correct: there this is a green-before/green-after REGRESSION GUARD that the
// two lanes agree on the rule-skip, not a reproduction.
func (s *IntegrationTestSuite) TestPlainFailureReleasesAllDoneConsumerDistributed() {
	if !distributedLane() {
		s.T().Skip("the plain (unfanned) consumer is only stranded where dispatch reads the SQL rows; the local lane's half is TestPlainFailureReleasesAllDoneFannedConsumer")
	}

	alias := fmt.Sprintf("plain-failure-plain-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(plainFailureManifest(alias, false))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, triggerRuleFailureRunTimeout)
	s.Equal("failed", run.Status, "a failed plain step must fail the run")

	s.assertPlainFailureResolvedSuccessors(job.ID, runID)
}
