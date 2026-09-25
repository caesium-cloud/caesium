//go:build integration

package robustness

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/test/robustness/cluster"
)

func runTerminalNoRegress(t *testing.T, fe *faultEnv) {
	refreshTopo(t, fe)
	ctx := context.Background()
	member := fe.leader
	job, alias := applyFaultFixture(t, fe, member, "terminal", 2)
	run, _, err := fe.httpAPI.TriggerRun(ctx, member.HTTPBase(), job.ID)
	if err != nil {
		t.Fatalf("trigger %s: %v", alias, err)
	}
	termCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	final := waitRunTerminal(t, termCtx, fe.httpAPI, member.HTTPBase(), job.ID, run.ID)
	cancel()
	if !strings.EqualFold(final.Status, "succeeded") {
		t.Fatalf("run %s ended %s, want succeeded", run.ID, final.Status)
	}
	before := fingerprintRun(t, ctx, fe, member.HTTPBase(), job.ID, run.ID)
	lease, err := fe.httpAPI.QueryLease(ctx, member.HTTPBase(), run.ID)
	if err != nil {
		t.Fatalf("lease after success: %v", err)
	}
	owner := memberByNode(t, fe, lease.OwnerNode)
	cli := validInternalClient(t, fe, owner)
	payload := completePayload(final, lease, "failed", lease.Generation)
	ex := cli.Complete(ctx, cluster.InternalBase(owner.IP), payload)
	if ex.Err != "" {
		t.Fatalf("stale terminal complete transport: %v", ex.Err)
	}
	requireHTTPStatus(t, ex.Status, http.StatusConflict, ex.Body)
	code, msg := ParseRefusal(ex.Status, []byte(ex.Body))
	if !TerminalCompleteRefusalAllowed(ex.Status, code) {
		t.Fatalf("duplicate complete did not hit the terminal/claim fence: status=%d code=%q msg=%s",
			ex.Status, code, RedactSecrets(msg))
	}
	after := fingerprintRun(t, ctx, fe, member.HTTPBase(), job.ID, run.ID)
	if !strings.EqualFold(after.Status, "succeeded") {
		t.Fatalf("terminal outcome regressed from succeeded to %s", after.Status)
	}
	requireNoMutation(t, before, after, "second terminal complete")
	writeCoreRecord(t, fe, "terminal_no_regress", map[string]any{
		"run_id":          run.ID,
		"owner":           owner.Name,
		"public_status":   after.Status,
		"complete_status": ex.Status,
		"refusal_code":    code,
		"complete_body":   RedactSecrets(ex.Body),
		"fingerprint":     digestOf(after),
	})
}

func runFrozenRetry(t *testing.T, fe *faultEnv) {
	refreshTopo(t, fe)
	ctx := context.Background()
	member := fe.leader
	alias := uniqueAlias("retry")
	job := applyCoreFixture(t, fe, member, "fail-then-retry.job.yaml", alias)
	run, _, err := fe.httpAPI.TriggerRun(ctx, member.HTTPBase(), job.ID)
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	failCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	_ = waitRunStatus(t, failCtx, fe.httpAPI, member.HTTPBase(), job.ID, run.ID, "failed")
	cancel()
	recipes, err := fe.httpAPI.QueryTaskRecipes(ctx, member.HTTPBase(), run.ID)
	if err != nil || len(recipes) == 0 {
		t.Fatalf("inconclusive: frozen recipes unread: %v", err)
	}
	original := map[string]cluster.TaskRecipe{}
	var boom cluster.TaskRecipe
	steps, err := taskStepNames(ctx, fe.httpAPI, member.HTTPBase(), job.ID)
	if err != nil {
		t.Fatalf("catalog tasks: %v", err)
	}
	for _, r := range recipes {
		original[r.ID] = r
		if steps[r.TaskID] == retryBoomStep {
			boom = r
		}
	}
	if boom.ID == "" {
		t.Fatalf("could not identify boom task recipe: steps=%v recipes=%d", steps, len(recipes))
	}

	edited, err := LoadCoreFixture("fail-then-retry.job.yaml", alias, fe.env.TaskImage+"-edited-must-not-run")
	if err != nil {
		t.Fatalf("edited fixture: %v", err)
	}
	for i := range edited.Steps {
		if edited.Steps[i].Name == retryBoomStep {
			edited.Steps[i].Command = []string{"sh", "-c", "echo edited-recipe; exit 1"}
			edited.Steps[i].Image = fe.env.TaskImage + "-edited-must-not-run"
		}
	}
	if err := fe.httpAPI.Apply(ctx, member.HTTPBase(), []jobdef.Definition{edited}); err != nil {
		t.Fatalf("apply edited jobdef: %v", err)
	}

	status, retried, raw, err := fe.httpAPI.RetryRun(ctx, member.HTTPBase(), job.ID, run.ID)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if status != http.StatusAccepted {
		t.Fatalf("retry status %d, want 202: %s", status, truncate(raw, 400))
	}
	if retried.ID != run.ID && retried.ID != "" {
		t.Logf("retry returned run %s (original %s)", retried.ID, run.ID)
	}

	retryCtx, retryCancel := context.WithTimeout(ctx, 5*time.Minute)
	afterRetry := waitRunTerminal(t, retryCtx, fe.httpAPI, member.HTTPBase(), job.ID, run.ID)
	retryCancel()
	if !strings.EqualFold(afterRetry.Status, "failed") {
		t.Fatalf("retried frozen boom recipe ended %s, want failed", afterRetry.Status)
	}
	if n := len(fe.sink.StartsFor(run.ID, retryBoomStep)); n < 2 {
		t.Fatalf("boom step started %d time(s), want >= 2 so the 202 retry actually re-ran", n)
	}

	frozen, err := fe.httpAPI.QueryTaskRecipes(ctx, member.HTTPBase(), run.ID)
	if err != nil {
		t.Fatalf("recipes after retry: %v", err)
	}
	for _, r := range frozen {
		orig, ok := original[r.ID]
		if !ok {
			continue
		}
		fpOrig := TaskFingerprint{ID: orig.ID, Image: orig.Image, Command: orig.Command}
		fpNow := TaskFingerprint{ID: r.ID, Image: r.Image, Command: r.Command}
		if err := FrozenRecipeChanged(fpOrig, fpNow); err != nil {
			t.Fatalf("DT-RETRY-01: %v", err)
		}
		if strings.Contains(r.Image, "edited-must-not-run") || strings.Contains(r.Command, "edited-recipe") {
			t.Fatalf("retried task used the edited jobdef recipe: image=%s command=%s", r.Image, r.Command)
		}
	}

	beforeReject := fingerprintRun(t, ctx, fe, member.HTTPBase(), job.ID, run.ID)
	runningJob, running, _ := applyBlockedRun(t, fe, member, "retry-running")
	if !strings.EqualFold(running.Status, "running") {
		t.Fatalf("rejected-retry control run %s is %s, want running", running.ID, running.Status)
	}
	beforeRunning := fingerprintRun(t, ctx, fe, member.HTTPBase(), runningJob.ID, running.ID)
	rejStatus, _, rejRaw, err := fe.httpAPI.RetryRun(ctx, member.HTTPBase(), runningJob.ID, running.ID)
	if err != nil {
		t.Fatalf("rejected retry transport: %v", err)
	}
	requireHTTPStatus(t, rejStatus, http.StatusConflict, string(rejRaw))
	afterRunning := fingerprintRun(t, ctx, fe, member.HTTPBase(), runningJob.ID, running.ID)
	requireNoMutation(t, beforeRunning, afterRunning, "retry of running run")
	afterFailed := fingerprintRun(t, ctx, fe, member.HTTPBase(), job.ID, run.ID)
	requireNoMutation(t, beforeReject, afterFailed, "retry of a different run")

	writeCoreRecord(t, fe, "frozen_retry_recipe", map[string]any{
		"failed_run":      run.ID,
		"retry_status":    status,
		"after_retry":     afterRetry.Status,
		"boom_starts":     len(fe.sink.StartsFor(run.ID, retryBoomStep)),
		"original_boom":   boom,
		"rejected_run":    running.ID,
		"rejected_status": rejStatus,
		"rejected_body":   RedactSecrets(string(rejRaw)),
		"frozen_digest":   digestOf(frozen),
	})
}

func runFanIn(t *testing.T, fe *faultEnv) {
	refreshTopo(t, fe)
	ctx := context.Background()
	member := fe.leader
	alias := uniqueAlias("fanin")
	job := applyCoreFixture(t, fe, member, "fan-in.job.yaml", alias)
	run, _, err := fe.httpAPI.TriggerRun(ctx, member.HTTPBase(), job.ID)
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}

	names, err := taskStepNames(ctx, fe.httpAPI, member.HTTPBase(), job.ID)
	if err != nil {
		t.Fatalf("catalog tasks: %v", err)
	}
	leftCtx, leftCancel := context.WithTimeout(ctx, 3*time.Minute)
	if err := cluster.Poll(leftCtx, time.Second, func() (bool, error) {
		got, gerr := fe.httpAPI.GetRun(leftCtx, member.HTTPBase(), job.ID, run.ID)
		if gerr != nil {
			return false, nil
		}
		return strings.EqualFold(taskStatusByName(got, names, fanInLeftStep), "succeeded"), nil
	}); err != nil {
		leftCancel()
		t.Fatalf("left predecessor never reached succeeded: %v", err)
	}
	leftCancel()

	holdUntil := time.Now().Add(25 * time.Second)
	for time.Now().Before(holdUntil) {
		if n := len(fe.sink.StartsFor(run.ID, fanInJoinStep)); n != 0 {
			t.Fatalf("join started after only left succeeded (%d starts); fan-in cannot be inferred from one predecessor", n)
		}
		got, gerr := fe.httpAPI.GetRun(ctx, member.HTTPBase(), job.ID, run.ID)
		if gerr == nil {
			st := taskStatusByName(got, names, fanInJoinStep)
			if st != "" && !strings.EqualFold(st, "pending") {
				t.Fatalf("join task status %q after only left succeeded, want pending", st)
			}
		}
		if n := len(fe.sink.CompletionsFor(run.ID, fanInRightStep)); n != 0 {
			t.Fatalf("right completed before the barrier was released")
		}
		time.Sleep(2 * time.Second)
	}

	fe.sink.Release(run.ID)
	joinCtx, joinCancel := context.WithTimeout(ctx, 5*time.Minute)
	final := waitRunTerminal(t, joinCtx, fe.httpAPI, member.HTTPBase(), job.ID, run.ID)
	joinCancel()
	if !strings.EqualFold(final.Status, "succeeded") {
		t.Fatalf("fan-in run ended %s", final.Status)
	}
	if err := FanInStartedTooEarly(timedEffects(fe.sink, run.ID), fanInJoinStep, []string{fanInLeftStep, fanInRightStep}); err != nil {
		t.Fatalf("DT-DAG-01: %v events=%s", err, eventsJSON(fe.sink))
	}

	writeCoreRecord(t, fe, "fan_in_predecessors", map[string]any{
		"run_id":         run.ID,
		"public_status":  final.Status,
		"left_complete":  len(fe.sink.CompletionsFor(run.ID, fanInLeftStep)),
		"right_complete": len(fe.sink.CompletionsFor(run.ID, fanInRightStep)),
		"join_starts":    len(fe.sink.StartsFor(run.ID, fanInJoinStep)),
	})
}

func runCancelCompletionRace(t *testing.T, fe *faultEnv) {
	refreshTopo(t, fe)
	ctx := context.Background()
	member := fe.leader
	alias := uniqueAlias("cancel")
	job := applyCoreFixture(t, fe, member, "replace-concurrency.job.yaml", alias)
	first, rawFirst, err := fe.httpAPI.TriggerRun(ctx, member.HTTPBase(), job.ID)
	if err != nil {
		t.Fatalf("trigger first: %v", err)
	}
	startCtx, startCancel := context.WithTimeout(ctx, 2*time.Minute)
	if err := cluster.Poll(startCtx, 500*time.Millisecond, func() (bool, error) {
		return len(fe.sink.StartsFor(first.ID, cluster.BlockStep)) > 0, nil
	}); err != nil {
		startCancel()
		t.Fatalf("first run never started block: %v", err)
	}
	startCancel()
	claimCtx, claimCancel := context.WithTimeout(ctx, time.Minute)
	firstTask, claimed := waitClaimedBy(t, claimCtx, fe, member.HTTPBase(), job.ID, first.ID, "")
	claimCancel()
	if !claimed || !strings.EqualFold(firstTask.Status, "running") || firstTask.Attempt < 1 {
		t.Fatalf("inconclusive: old block task has no active completion claim: claimed=%t task=%+v", claimed, firstTask)
	}
	names, err := taskStepNames(ctx, fe.httpAPI, member.HTTPBase(), job.ID)
	if err != nil {
		t.Fatalf("catalog tasks for cancelled run: %v", err)
	}
	if names[firstTask.TaskID] != cluster.BlockStep {
		t.Fatalf("inconclusive: claimed old task %s is %q, want block", firstTask.ID, names[firstTask.TaskID])
	}
	firstLease, err := fe.httpAPI.QueryLease(ctx, member.HTTPBase(), first.ID)
	if err != nil || firstLease.Generation < 1 || strings.TrimSpace(firstLease.OwnerNode) == "" {
		t.Fatalf("inconclusive: old block completion lease unavailable: lease=%+v err=%v", firstLease, err)
	}
	oldOwner := memberByNode(t, fe, firstLease.OwnerNode)
	oldClient := validInternalClient(t, fe, oldOwner)
	oldComplete := map[string]any{
		"run_id": first.ID, "task_id": firstTask.TaskID, "task_run_id": firstTask.ID,
		"owner_generation": firstLease.Generation, "attempt": firstTask.Attempt,
		"worker_node": firstTask.ClaimedBy, "status": "succeeded", "result": "success",
	}

	second, rawSecond, err := fe.httpAPI.TriggerRun(ctx, member.HTTPBase(), job.ID)
	if err != nil {
		t.Fatalf("trigger replacement: %v", err)
	}
	if second.ID == first.ID {
		t.Fatalf("replace admission returned the same run %s", first.ID)
	}
	t.Logf("replacement 202 run %s (first %s); 202 is not process-death ack", second.ID, first.ID)

	// Release the first barrier while cancel is in flight so a completion can
	// race. The second run has its own wait key and is released so it can finish.
	fe.sink.Release(first.ID)
	fe.sink.Release(second.ID)
	completionRace := oldClient.Complete(ctx, cluster.InternalBase(oldOwner.IP), oldComplete)
	if completionRace.Err != "" {
		t.Fatalf("inconclusive: old completion contender did not reach owner: %s", completionRace.Err)
	}
	refusalCode, refusalMessage := ParseRefusal(completionRace.Status, []byte(completionRace.Body))
	leaseAbsenceProven := false
	if completionRace.Status == http.StatusConflict && refusalCode == RefusalMissingRun {
		// Concurrency replacement deletes the old lease in the same transaction
		// as cancellation. The handler also maps an unavailable lease read to
		// missing_run, so corroborate this response on the responding node.
		absent, qerr := fe.httpAPI.QueryLeaseAbsent(ctx, oldOwner.HTTPBase(), first.ID)
		if qerr != nil {
			t.Fatalf("inconclusive: missing_run without healthy SQL lease read: %v", qerr)
		}
		if !absent {
			t.Fatalf("old completion returned missing_run while lease %s still exists", first.ID)
		}
		leaseAbsenceProven = true
	}
	if !CancelledCompleteRefusalAllowed(completionRace.Status, refusalCode) && !leaseAbsenceProven {
		t.Fatalf("old completion contender was not fenced after replacement: status=%d code=%q body=%s",
			completionRace.Status, refusalCode, RedactSecrets(refusalMessage))
	}

	cancelCtx, cancelDone := context.WithTimeout(ctx, 2*time.Minute)
	var firstAfter cluster.Run
	if err := cluster.Poll(cancelCtx, time.Second, func() (bool, error) {
		got, gerr := fe.httpAPI.GetRun(cancelCtx, member.HTTPBase(), job.ID, first.ID)
		if gerr != nil {
			return false, nil
		}
		firstAfter = got
		return strings.EqualFold(got.Status, "cancelled"), nil
	}); err != nil {
		cancelDone()
		t.Fatalf("first run %s did not stay/become cancelled after replace (status=%s): %v", first.ID, firstAfter.Status, err)
	}
	cancelDone()
	if !strings.EqualFold(firstAfter.Status, "cancelled") {
		t.Fatalf("first run status %s, want cancelled", firstAfter.Status)
	}

	completesAtCancel := len(fe.sink.CompletionsFor(first.ID, cluster.BlockStep))
	checkCancelled := func(got cluster.Run, when string) {
		if !strings.EqualFold(got.Status, "cancelled") {
			t.Fatalf("%s: cancelled run regressed to %s", when, got.Status)
		}
		recipes, rerr := fe.httpAPI.QueryTaskRecipes(ctx, member.HTTPBase(), first.ID)
		if rerr != nil {
			t.Fatalf("%s: durable task rows unavailable: %v", when, rerr)
		}
		byID := make(map[string]cluster.TaskRecipe, len(recipes))
		durableIDs := make([]string, 0, len(recipes))
		for _, r := range recipes {
			byID[r.ID] = r
			durableIDs = append(durableIDs, r.ID+"/"+r.TaskID+"/"+r.Status)
		}
		if len(got.Tasks) != 2 {
			t.Fatalf("%s: cancelled run has %d task rows, want 2", when, len(got.Tasks))
		}
		if len(byID) != 2 || len(recipes) != 2 {
			t.Fatalf("%s: cancelled run has %d durable task rows (%d distinct), want 2", when, len(recipes), len(byID))
		}
		seen := map[string]bool{}
		for _, tr := range got.Tasks {
			step := names[tr.TaskID]
			if step != cluster.BlockStep && step != "successor" {
				t.Fatalf("%s: unexpected task %s (%s)", when, tr.TaskID, step)
			}
			if seen[step] {
				t.Fatalf("%s: duplicate %s task row", when, step)
			}
			seen[step] = true
			durable, ok := byID[tr.ID]
			if !ok || durable.TaskID != tr.TaskID {
				t.Fatalf("%s: public %s task id=%s task_id=%s has no matching durable row (id_found=%t matched_task_id=%s durable_ids=%v)",
					when, step, tr.ID, tr.TaskID, ok, durable.TaskID, durableIDs)
			}
			if !strings.EqualFold(tr.Status, "cancelled") || !strings.EqualFold(durable.Status, "cancelled") ||
				strings.TrimSpace(tr.ClaimedBy) != "" || strings.TrimSpace(durable.ClaimedBy) != "" {
				t.Fatalf("%s: old %s task public=%s/%q durable=%s/%q, want cancelled and unclaimed",
					when, step, tr.Status, tr.ClaimedBy, durable.Status, durable.ClaimedBy)
			}
		}
		if !seen[cluster.BlockStep] || !seen["successor"] {
			t.Fatalf("%s: cancelled run is missing a block or successor task: %v", when, seen)
		}
		if n := len(fe.sink.StartsFor(first.ID, "successor")); n != 0 {
			t.Fatalf("%s: old run started successor %d time(s) after replace", when, n)
		}
	}
	checkCancelled(firstAfter, "at cancel")
	time.Sleep(15 * time.Second)
	completesAfterCancel := len(fe.sink.CompletionsFor(first.ID, cluster.BlockStep))
	still, err := fe.httpAPI.GetRun(ctx, member.HTTPBase(), job.ID, first.ID)
	if err != nil {
		t.Fatalf("re-read cancelled run: %v", err)
	}
	checkCancelled(still, "after completion race")

	secCtx, secCancel := context.WithTimeout(ctx, 5*time.Minute)
	secondFinal := waitRunStatus(t, secCtx, fe.httpAPI, member.HTTPBase(), job.ID, second.ID, "succeeded")
	secCancel()

	writeCoreRecord(t, fe, "cancel_completion_race", map[string]any{
		"first_run":                first.ID,
		"second_run":               second.ID,
		"first_status":             still.Status,
		"second_status":            secondFinal.Status,
		"first_raw_admit":          truncate(rawFirst, 200),
		"second_raw_admit":         truncate(rawSecond, 200),
		"completes_at_cancel":      completesAtCancel,
		"completes_after":          completesAfterCancel,
		"old_complete_status":      completionRace.Status,
		"old_complete_code":        refusalCode,
		"old_lease_absence_proven": leaseAbsenceProven,
		"successor_starts":         len(fe.sink.StartsFor(first.ID, "successor")),
		"replacement_202_ack":      "not_process_death",
		"histories":                []string{first.ID, second.ID},
	})
}
