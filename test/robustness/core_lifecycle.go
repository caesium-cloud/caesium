//go:build integration

package robustness

import (
	"context"
	"encoding/json"
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
	payload := completePayload(t, fe, member.HTTPBase(), final, lease, "failed", lease.Generation)
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

type cancelContender struct {
	member      cluster.Member
	job         cluster.Job
	first       cluster.Run
	rawFirst    []byte
	firstTask   cluster.Task
	blockRecipe cluster.TaskRecipe
	firstLease  cluster.Lease
	oldOwner    cluster.Member
	oldClient   *cluster.InternalClient
	oldComplete map[string]any
	names       map[string]string
}

func prepareCancelContender(t *testing.T, fe *faultEnv) cancelContender {
	t.Helper()
	refreshTopo(t, fe)
	ctx := context.Background()
	member := fe.leader
	alias := uniqueAlias("cancel")
	job := applyCoreFixture(t, fe, member, "replace-concurrency.job.yaml", alias)
	first, rawFirst, err := fe.httpAPI.TriggerRun(ctx, member.HTTPBase(), job.ID)
	if err != nil {
		t.Fatalf("trigger first: %v", err)
	}
	t.Cleanup(func() { fe.sink.Release(first.ID) })
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
	recipes, err := fe.httpAPI.QueryTaskRecipes(ctx, member.HTTPBase(), first.ID)
	if err != nil {
		t.Fatalf("inconclusive: old block durable claim unavailable: %v", err)
	}
	block, err := cluster.ResolveUnfannedTaskRecipe(firstTask, recipes)
	if err != nil || !strings.EqualFold(block.Status, "running") ||
		block.ClaimAttempt < 1 || block.OwnerGeneration != firstLease.Generation {
		t.Fatalf("inconclusive: old block claim is not authoritative for current lease: public=%+v durable=%+v lease=%+v err=%v", firstTask, block, firstLease, err)
	}
	oldOwner := memberByNode(t, fe, firstLease.OwnerNode)
	oldClient := validInternalClient(t, fe, oldOwner)
	completionNonce := uniqueAlias("complete")
	oldComplete := map[string]any{
		"run_id": first.ID, "task_id": firstTask.TaskID, "task_run_id": block.ID,
		"owner_generation": firstLease.Generation, "attempt": firstTask.Attempt,
		"worker_node": firstTask.ClaimedBy, "status": "succeeded", "result": "success",
		"outputs": map[string]string{"cancel_race_nonce": completionNonce},
	}
	return cancelContender{
		member: member, job: job, first: first, rawFirst: rawFirst,
		firstTask: firstTask, blockRecipe: block, firstLease: firstLease, oldOwner: oldOwner,
		oldClient: oldClient, oldComplete: oldComplete, names: names,
	}
}

// runCancelPostCommitFence starts the old completion after the replacement's
// 202, so it proves the durable post-cancel fence, not transaction overlap.
func runCancelPostCommitFence(t *testing.T, fe *faultEnv) {
	c := prepareCancelContender(t, fe)
	ctx := context.Background()
	member, job, first, rawFirst := c.member, c.job, c.first, c.rawFirst
	oldOwner, oldClient, oldComplete, names := c.oldOwner, c.oldClient, c.oldComplete, c.names

	second, rawSecond, err := fe.httpAPI.TriggerRun(ctx, member.HTTPBase(), job.ID)
	if err != nil {
		t.Fatalf("trigger replacement: %v", err)
	}
	if second.ID == first.ID {
		t.Fatalf("replace admission returned the same run %s", first.ID)
	}
	t.Logf("replacement 202 run %s (first %s); 202 is not process-death ack", second.ID, first.ID)

	// The replacement has already committed. Releasing this barrier only lets
	// the external process finish; it cannot turn the completion into a race
	// with the cancellation transaction.
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
		if err := checkCancelledRaceTaskSet(got, recipes, names, first.ID, c.firstTask, c.blockRecipe); err != nil {
			t.Fatalf("%s: cancelled run task set: %v", when, err)
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
	checkCancelled(still, "after post-commit completion")

	secCtx, secCancel := context.WithTimeout(ctx, 5*time.Minute)
	secondFinal := waitRunStatus(t, secCtx, fe.httpAPI, member.HTTPBase(), job.ID, second.ID, "succeeded")
	secCancel()

	writeCoreRecord(t, fe, "cancel_post_commit_fence", map[string]any{
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
		"completion_phase":         "post_cancel_commit",
		"histories":                []string{first.ID, second.ID},
	})
}

// runCancelCompletionRace sends both operations through a shared transport
// barrier. Neither request can reach a server until BOTH invocation intervals
// have begun. Persisted events on the old run decide which write committed
// first; client response arrival time is not used as server ordering evidence.
func runCancelCompletionRace(t *testing.T, fe *faultEnv) {
	c := prepareCancelContender(t, fe)
	gate := newRequestRaceGate()
	defer gate.releaseBoth()
	runCtx, runCancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer runCancel()

	publicClient := *fe.httpAPI.Client
	publicClient.Transport = gate.transport("replace", publicClient.Transport)
	publicAPI := *fe.httpAPI
	publicAPI.Client = &publicClient
	internalClient := *c.oldClient
	internalHTTP := *c.oldClient.HTTP
	internalHTTP.Transport = gate.transport("complete", internalHTTP.Transport)
	internalClient.HTTP = &internalHTTP

	type triggerResult struct {
		run cluster.Run
		raw []byte
		err error
	}
	triggered := make(chan triggerResult, 1)
	completed := make(chan cluster.InternalExchange, 1)
	go func() {
		run, raw, err := publicAPI.TriggerRun(runCtx, c.member.HTTPBase(), c.job.ID)
		triggered <- triggerResult{run: run, raw: raw, err: err}
	}()
	go func() {
		completed <- internalClient.Complete(runCtx, cluster.InternalBase(c.oldOwner.IP), c.oldComplete)
	}()
	readyCtx, readyCancel := context.WithTimeout(runCtx, 10*time.Second)
	arrivals, err := gate.awaitBoth(readyCtx)
	readyCancel()
	if err != nil {
		runCancel()
		t.Fatalf("inconclusive: replacement and completion did not both enter the race: %v", err)
	}
	// The requests are still held locally. Recheck the exact durable claim and
	// lease immediately before delivery so an intervening recovery is not
	// mistaken for a cancellation/completion race on the prepared claim.
	gateLease, err := fe.httpAPI.QueryLease(runCtx, c.member.HTTPBase(), c.first.ID)
	if err != nil || gateLease.OwnerNode != c.firstLease.OwnerNode || gateLease.Generation != c.firstLease.Generation {
		t.Fatalf("inconclusive: old lease changed before race release: initial=%+v gate=%+v err=%v", c.firstLease, gateLease, err)
	}
	gateRecipes, err := fe.httpAPI.QueryTaskRecipes(runCtx, c.member.HTTPBase(), c.first.ID)
	if err != nil {
		t.Fatalf("inconclusive: old claim unavailable before race release: %v", err)
	}
	gateBlock, err := cluster.ResolveUnfannedTaskRecipe(c.firstTask, gateRecipes)
	if err != nil || gateBlock.ID != c.blockRecipe.ID || gateBlock.ClaimAttempt != c.blockRecipe.ClaimAttempt ||
		gateBlock.OwnerGeneration != gateLease.Generation || gateBlock.ResultDigest != c.blockRecipe.ResultDigest ||
		gateBlock.OutputDigest != c.blockRecipe.OutputDigest {
		t.Fatalf("inconclusive: prepared block claim changed before race release: initial=%+v gate=%+v err=%v", c.blockRecipe, gateBlock, err)
	}
	releasedAt := gate.releaseBoth()
	for _, arrival := range arrivals {
		if !arrival.At.Before(releasedAt) {
			t.Fatalf("inconclusive: %s was not invoked before the shared gate opened", arrival.Name)
		}
	}
	var replacement triggerResult
	select {
	case replacement = <-triggered:
	case <-runCtx.Done():
		t.Fatalf("inconclusive: replacement did not settle: %v", runCtx.Err())
	}
	if replacement.err != nil {
		t.Fatalf("inconclusive: replacement admission: %v", replacement.err)
	}
	if replacement.run.ID == c.first.ID {
		t.Fatalf("replacement admission reused old run %s", c.first.ID)
	}
	second := replacement.run
	t.Cleanup(func() { fe.sink.Release(second.ID) })
	var contender cluster.InternalExchange
	select {
	case contender = <-completed:
	case <-runCtx.Done():
		t.Fatalf("inconclusive: completion contender did not settle: %v", runCtx.Err())
	}
	if contender.Err != "" {
		t.Fatalf("inconclusive: completion contender transport: %s", contender.Err)
	}
	code, message := ParseRefusal(contender.Status, []byte(contender.Body))
	if contender.Status == http.StatusOK {
		var accepted struct {
			Accepted bool `json:"accepted"`
		}
		if err := json.Unmarshal([]byte(contender.Body), &accepted); err != nil || !accepted.Accepted {
			t.Fatalf("inconclusive: completion 200 lacks accepted=true: body=%s err=%v", RedactSecrets(contender.Body), err)
		}
	}
	if contender.Status != http.StatusOK && contender.Status != http.StatusConflict {
		t.Fatalf("completion contender returned unsupported outcome %d/%q: %s", contender.Status, code, RedactSecrets(message))
	}

	settleCtx, settleCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	firstFinal := waitRunTerminal(t, settleCtx, fe.httpAPI, c.member.HTTPBase(), c.job.ID, c.first.ID)
	settleCancel()
	recipes, err := fe.httpAPI.QueryTaskRecipes(context.Background(), c.member.HTTPBase(), c.first.ID)
	if err != nil {
		t.Fatalf("inconclusive: durable old task rows after race: %v", err)
	}
	var publicBlock cluster.Task
	publicBlockCount := 0
	for _, task := range firstFinal.Tasks {
		if task.TaskID == c.firstTask.TaskID {
			publicBlock = task
			publicBlockCount++
		}
	}
	if publicBlockCount != 1 {
		t.Fatalf("inconclusive: raced block has %d public rows, want one", publicBlockCount)
	}
	block, err := cluster.ResolveUnfannedTaskRecipe(publicBlock, recipes)
	if err != nil || block.ID != c.blockRecipe.ID {
		t.Fatalf("inconclusive: raced block instance changed: prepared=%+v public=%+v durable=%+v err=%v", c.blockRecipe, publicBlock, block, err)
	}
	eventCtx, eventCancel := context.WithTimeout(context.Background(), 20*time.Second)
	rows, scope, err := readPersistedEvents(eventCtx, fe.httpAPI, c.member.HTTPBase(), c.first.ID, 2000)
	eventCancel()
	if err != nil || !scope.Complete {
		t.Fatalf("inconclusive: old run's durable event order unavailable: scope=%+v err=%v", scope, err)
	}
	events := make([]CancelRaceEvent, 0, len(rows))
	for _, row := range rows {
		events = append(events, CancelRaceEvent{Sequence: row.Sequence, Type: row.Type, RunID: row.RunID, TaskID: row.TaskID})
	}
	outcome, err := ClassifyCancelCompletionRace(c.first.ID, c.firstTask.TaskID, contender.Status, code,
		firstFinal.Status, block.Status, events)
	if err != nil {
		t.Fatalf("cancel/completion race violated durable order: %v (status=%d/%q run=%s block=%s events=%+v)",
			err, contender.Status, code, firstFinal.Status, block.Status, events)
	}
	if outcome == "cancellation_won" {
		requireCancelledRaceTaskSet(t, fe, c, firstFinal, recipes, "at race settlement")
	}
	if contender.Status == http.StatusConflict {
		// The old task was still blocked, so a rejected synthetic completion
		// must leave its result and output exactly as they were before the race.
		if block.ResultDigest != c.blockRecipe.ResultDigest || block.OutputDigest != c.blockRecipe.OutputDigest ||
			strings.TrimSpace(block.ClaimedBy) != "" {
			t.Fatalf("rejected completion mutated old task payload or retained its claim: before=%+v after=%+v", c.blockRecipe, block)
		}
	} else if block.ResultDigest == c.blockRecipe.ResultDigest || block.OutputDigest == c.blockRecipe.OutputDigest {
		t.Fatalf("accepted completion lacked its durable result/output: before=%+v after=%+v", c.blockRecipe, block)
	}

	fe.sink.Release(c.first.ID)
	fe.sink.Release(second.ID)
	secCtx, secCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	secondFinal := waitRunStatus(t, secCtx, fe.httpAPI, c.member.HTTPBase(), c.job.ID, second.ID, "succeeded")
	secCancel()
	time.Sleep(15 * time.Second)
	still, err := fe.httpAPI.GetRun(context.Background(), c.member.HTTPBase(), c.job.ID, c.first.ID)
	if err != nil || !strings.EqualFold(still.Status, firstFinal.Status) {
		t.Fatalf("old run terminal status regressed after releasing task: first=%s later=%s err=%v", firstFinal.Status, still.Status, err)
	}
	lateRecipes, err := fe.httpAPI.QueryTaskRecipes(context.Background(), c.member.HTTPBase(), c.first.ID)
	if err != nil {
		t.Fatalf("inconclusive: old task rows unavailable after barrier release: %v", err)
	}
	latePublicBlockCount := 0
	var latePublicBlock cluster.Task
	for _, task := range still.Tasks {
		if task.TaskID == block.TaskID {
			latePublicBlock = task
			latePublicBlockCount++
		}
	}
	if latePublicBlockCount != 1 {
		t.Fatalf("inconclusive: old block has %d public rows after barrier release, want one", latePublicBlockCount)
	}
	lateBlock, err := cluster.ResolveUnfannedTaskRecipe(latePublicBlock, lateRecipes)
	if err != nil || lateBlock.ID != block.ID {
		t.Fatalf("inconclusive: old block identity changed after barrier release: before=%+v after=%+v err=%v", block, lateBlock, err)
	}
	if lateBlock.Status != block.Status || lateBlock.ClaimedBy != block.ClaimedBy ||
		lateBlock.ResultDigest != block.ResultDigest || lateBlock.OutputDigest != block.OutputDigest {
		t.Fatalf("old task changed after releasing blocked worker: before=%+v after=%+v", block, lateBlock)
	}
	if outcome == "cancellation_won" {
		requireCancelledRaceTaskSet(t, fe, c, still, lateRecipes, "after barrier release")
	}
	lateRows, lateScope, err := readPersistedEvents(context.Background(), fe.httpAPI, c.member.HTTPBase(), c.first.ID, 2000)
	if err != nil || !lateScope.Complete {
		t.Fatalf("inconclusive: old run's late durable event order unavailable: scope=%+v err=%v", lateScope, err)
	}
	lateEvents := make([]CancelRaceEvent, 0, len(lateRows))
	for _, row := range lateRows {
		lateEvents = append(lateEvents, CancelRaceEvent{Sequence: row.Sequence, Type: row.Type, RunID: row.RunID, TaskID: row.TaskID})
	}
	lateOutcome, err := ClassifyCancelCompletionRace(c.first.ID, c.firstTask.TaskID, contender.Status, code,
		still.Status, block.Status, lateEvents)
	if err != nil || lateOutcome != outcome {
		t.Fatalf("old run's race outcome changed after barrier release: first=%s later=%s err=%v", outcome, lateOutcome, err)
	}
	writeCoreRecord(t, fe, "cancel_completion_race", map[string]any{
		"first_run": c.first.ID, "second_run": second.ID,
		"first_status": still.Status, "second_status": secondFinal.Status,
		"first_raw_admit": truncate(c.rawFirst, 200), "second_raw_admit": truncate(replacement.raw, 200),
		"completion_status": contender.Status, "completion_code": code,
		"outcome":       outcome,
		"gate_arrivals": arrivals, "gate_released_at": releasedAt,
		"persisted_scope": scope, "persisted_events": events,
		"late_persisted_scope": lateScope, "late_persisted_events": lateEvents,
		"histories": []string{c.first.ID, second.ID},
	})
}

func requireCancelledRaceTaskSet(t *testing.T, fe *faultEnv, c cancelContender,
	public cluster.Run, recipes []cluster.TaskRecipe, when string) {
	t.Helper()
	if err := checkCancelledRaceTaskSet(public, recipes, c.names, c.first.ID, c.firstTask, c.blockRecipe); err != nil {
		t.Fatalf("%s: cancellation winner task set: %v", when, err)
	}
	if starts := len(fe.sink.StartsFor(c.first.ID, "successor")); starts != 0 {
		t.Fatalf("%s: cancelled old run started successor %d time(s)", when, starts)
	}
}
