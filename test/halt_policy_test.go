//go:build integration

package test

import (
	"fmt"
	"os"
	"time"
)

// CAESIUM_TASK_FAILURE_POLICY=halt is the default, and every integration
// server runs with it. What it promises (docs/job-definitions.md, trigger
// rules): a failure stops the run from ADMITTING new work, but a step whose
// trigger rule is failure-tolerant (`all_done`, `always`, `one_success`,
// `all_failed`) still runs when its rule is satisfied, and work that had
// already started finishes on its own. Before #401 neither executor honoured
// the first half of that: the local Kahn loop cleared its whole queue on the
// first failure and the distributed waiter finalized the run before the
// released `all_done` successor could be claimed, so a cleanup step meant to
// run "no matter what" silently never did.
//
// The scenario drives the real surface on whichever lane the suite runs
// against — the local executor by default, the distributed owner+worker under
// `just integration-test-distributed`, and the in-memory run owner under
// `just integration-test-owner-memory` (where the sweep's rows have to be
// folded into the owner's RunState, or the released cleanup is never
// dispatched and the run never finishes) — and asserts EXECUTION, not just
// row resolution:
//
//	fail (exit 1) ──┬──▶ cleanup (all_done)                          → RUNS
//	                ├──▶ strict  (all_success)                       → skipped, rule reason
//	                └──▶ gate    (all_done) ──▶ after-gate (all_success) ──▶ late-cleanup (all_done)
//	                     ↑ RUNS                ↑ HALTED, never runs        ↑ RUNS (skipped pred satisfies all_done)
//	independent (root, all_success)                                  → ran if it started first, else halted
//
// `after-gate` is the deterministic "new work" probe: it cannot be released
// before `gate` completes, and `gate` cannot run before `fail` has failed, so
// it is provably not started when the halt lands — on either lane, whatever
// order a one-slot worker happens to claim roots in. Its own `all_done` child
// still runs because a skipped predecessor satisfies `all_done`.
//
// `independent` is a genuinely independent root. Whether it starts before
// `fail` fails is a dispatch-order race on the distributed lane (one worker
// slot, both roots ready at once), so the only lane-independent contract is:
// it either ran to completion (it had started) or it was halted with the halt
// reason — never left pending, never started after the failure.
const haltPolicyRunTimeout = 3 * time.Minute

func haltPolicyManifest(alias string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: fail
    image: alpine:3.23
    command: ["sh", "-c", "echo fail failing; exit 1"]
    next: [cleanup, strict, gate]
  - name: independent
    image: alpine:3.23
    command: ["sh", "-c", "echo independent ran"]
  - name: cleanup
    image: alpine:3.23
    command: ["sh", "-c", "echo cleanup ran"]
    dependsOn: [fail]
    triggerRule: all_done
  - name: strict
    image: alpine:3.23
    command: ["sh", "-c", "echo strict ran"]
    dependsOn: [fail]
  - name: gate
    image: alpine:3.23
    command: ["sh", "-c", "echo gate ran"]
    dependsOn: [fail]
    triggerRule: all_done
    next: [after-gate]
  - name: after-gate
    image: alpine:3.23
    command: ["sh", "-c", "echo after-gate ran"]
    dependsOn: [gate]
    next: [late-cleanup]
  - name: late-cleanup
    image: alpine:3.23
    command: ["sh", "-c", "echo late cleanup ran"]
    dependsOn: [after-gate]
    triggerRule: all_done
`, alias)
}

// TestHaltPolicyRunsTolerantSuccessorsAndHaltsIndependentWork is the
// end-to-end contract for the default failure policy on every lane.
func (s *IntegrationTestSuite) TestHaltPolicyRunsTolerantSuccessorsAndHaltsIndependentWork() {
	alias := fmt.Sprintf("halt-policy-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(haltPolicyManifest(alias))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, haltPolicyRunTimeout)
	s.Equal("failed", run.Status, "a halted run ends failed on the upstream failure, whatever the cleanups did")

	var detail plainFailureRun
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s", job.ID, runID), &detail)
	names := []string{"fail", "independent", "cleanup", "strict", "gate", "after-gate", "late-cleanup"}
	byName := map[string]plainFailureTask{}
	for _, name := range names {
		id := s.jobTaskIDByName(job.ID, name)
		for _, t := range detail.Tasks {
			if t.ID == id {
				byName[name] = t
			}
		}
	}
	s.Require().Len(byName, len(names), "every step must have a run task: %+v", detail.Tasks)
	s.Require().Equal("failed", byName["fail"].Status, "the fixture must actually fail `fail`: %+v", byName)

	haltReason := `run halted after task "fail" failed`

	// The tolerant successors of the failure EXECUTE under halt.
	s.Equal("succeeded", byName["cleanup"].Status,
		"an all_done successor of the failed step must run under the default halt policy: %+v", byName["cleanup"])
	s.Equal("succeeded", byName["gate"].Status,
		"an all_done successor of the failed step must run under the default halt policy: %+v", byName["gate"])

	// The intolerant successor is resolved by its rule.
	s.Equal("skipped", byName["strict"].Status)
	s.Equal(`trigger rule "all_success" not satisfied`, byName["strict"].Error)

	// New all_success work behind the tolerant gate is halted, not run — even
	// though its only predecessor succeeded.
	s.Equal("skipped", byName["after-gate"].Status,
		"all_success work that had not started when the failure landed is halted, not run: %+v", byName["after-gate"])
	s.Equal(haltReason, byName["after-gate"].Error,
		"the halt reason is user-visible and identical on every lane")

	// …but a tolerant step downstream of the halted step still runs: a
	// skipped predecessor satisfies all_done.
	s.Equal("succeeded", byName["late-cleanup"].Status,
		"an all_done step downstream of a halted step must run: %+v", byName["late-cleanup"])

	// The independent root either started before the failure (and finished)
	// or was halted with the halt reason. See the comment on the fixture.
	switch byName["independent"].Status {
	case "succeeded":
	case "skipped":
		s.Equal(haltReason, byName["independent"].Error,
			"a halted independent root carries the halt reason: %+v", byName["independent"])
	default:
		s.Failf("independent root in an unexpected state", "%+v", byName["independent"])
	}

	// Nothing is left pending on a terminal run.
	for name, t := range byName {
		s.NotEqual("pending", t.Status, "step %q left pending on a terminal run: %+v", name, t)
		s.NotEqual("running", t.Status, "step %q left running on a terminal run: %+v", name, t)
	}
}
