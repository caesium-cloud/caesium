//go:build integration

package robustness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/test/robustness/cluster"
	"github.com/caesium-cloud/caesium/test/robustness/faults"
	"github.com/google/uuid"
)

func runStaleGeneration(t *testing.T, fe *faultEnv) {
	refreshTopo(t, fe)
	ctx := context.Background()
	owner := fe.leader
	if cluster.IsControlPlaneNode(owner.Node) {
		t.Fatalf("owner %s is on control-plane node %s", owner.Name, owner.Node)
	}
	alias := uniqueAlias("stale")
	def := cluster.FixtureDefinition(alias, fe.env.TaskImage)
	if err := fe.httpAPI.Apply(ctx, owner.HTTPBase(), []jobdef.Definition{def}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	job, err := fe.httpAPI.JobByAlias(ctx, owner.HTTPBase(), alias)
	if err != nil {
		t.Fatalf("read job: %v", err)
	}

	cordonCtx, cordonCancel := context.WithTimeout(ctx, 60*time.Second)
	if _, err := cluster.RequestHost(cordonCtx, fe.kube, fe.env.Namespace, cluster.HostRequest{
		RequestID: uuid.NewString(), Action: cluster.ActionCordon,
		OwnerPod: owner.Name, OwnerKindNode: owner.Node,
	}); err != nil {
		cordonCancel()
		t.Fatalf("cordon %s: %v", owner.Node, err)
	}
	cordonCancel()
	t.Cleanup(func() {
		restartCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if _, err := cluster.RequestHost(restartCtx, fe.kube, fe.env.Namespace, cluster.HostRequest{
			RequestID: uuid.NewString(), Action: cluster.ActionRestart,
			OwnerPod: owner.Name, OwnerKindNode: owner.Node,
		}); err != nil {
			t.Logf("cleanup restart/uncordon %s: %v", owner.Node, err)
		}
	})

	run, _, err := fe.httpAPI.TriggerRun(ctx, owner.HTTPBase(), job.ID)
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	survivors := fe.topo.Survivors(owner)
	if len(survivors) != 2 {
		t.Fatalf("expected two survivors, got %d", len(survivors))
	}
	leaseBase := survivors[0].HTTPBase()
	leaseCtx, leaseCancel := context.WithTimeout(ctx, 90*time.Second)
	lease, err := cluster.WaitLease(leaseCtx, fe.httpAPI, leaseBase, run.ID, owner.NodeAddress)
	leaseCancel()
	if err != nil {
		t.Fatalf("lease before pause: %v", err)
	}
	leaseObservedAt := time.Now().UTC()
	startCtx, startCancel := context.WithTimeout(ctx, 90*time.Second)
	if err := cluster.Poll(startCtx, 500*time.Millisecond, func() (bool, error) {
		return len(fe.sink.StartsFor(run.ID, cluster.BlockStep)) > 0, nil
	}); err != nil {
		startCancel()
		t.Fatalf("blocked start missing: %v", err)
	}
	startCancel()
	initialBlockStarts := len(fe.sink.StartsFor(run.ID, cluster.BlockStep))
	names, err := taskStepNames(ctx, fe.httpAPI, leaseBase, job.ID)
	if err != nil {
		t.Fatalf("inconclusive: catalog task identities before takeover: %v", err)
	}
	var originalBlock cluster.TaskRecipe
	var lastInitialClaimError string
	initialClaimCtx, initialClaimCancel := context.WithTimeout(ctx, 60*time.Second)
	if err := cluster.Poll(initialClaimCtx, time.Second, func() (bool, error) {
		recipes, rerr := fe.httpAPI.QueryTaskRecipes(initialClaimCtx, leaseBase, run.ID)
		if rerr != nil {
			lastInitialClaimError = rerr.Error()
			return false, nil
		}
		originalBlock = cluster.TaskRecipe{}
		for _, recipe := range recipes {
			if names[recipe.TaskID] == cluster.BlockStep {
				originalBlock = recipe
				break
			}
		}
		if originalBlock.ID == "" || !strings.EqualFold(originalBlock.Status, "running") ||
			strings.TrimSpace(originalBlock.ClaimedBy) == "" || originalBlock.ClaimAttempt < 1 ||
			originalBlock.OwnerGeneration != lease.Generation {
			lastInitialClaimError = fmt.Sprintf("initial block lacks a durable owner claim: %+v lease=%+v", originalBlock, lease)
			return false, nil
		}
		return true, nil
	}); err != nil {
		initialClaimCancel()
		t.Fatalf("inconclusive: initial block claim was not observed: %v (last=%s)", err, lastInitialClaimError)
	}
	initialClaimCancel()

	live, err := cluster.RefreshMember(ctx, fe.kube, fe.env.Namespace, owner.Name)
	if err != nil || live.ContainerID == "" {
		t.Fatalf("refresh owner: %v", err)
	}
	beforeState, beforeRaw, err := fe.host.TaskState(ctx, live.Node, live.ContainerID)
	if err != nil {
		t.Fatalf("task state before pause: %v (%s)", err, truncate([]byte(beforeRaw), 200))
	}
	ev := faults.PauseEvidence{
		ContainerID: live.ContainerID, StateBefore: beforeState,
		LeaseTTLSeconds: coreLeaseTTLSeconds, ObservedNodeAddr: live.NodeAddress,
		ReachableBefore: fe.httpAPI.Health(ctx, live.HTTPBase()) == nil,
	}
	resumed := false
	t.Cleanup(func() {
		if resumed {
			return
		}
		cleanCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, _, _ = fe.host.Resume(cleanCtx, live.Node, live.ContainerID)
	})

	pauseCtx, pauseCancel := context.WithTimeout(ctx, 2*time.Minute)
	pausedState, pausedRaw, err := fe.host.Pause(pauseCtx, live.Node, live.ContainerID)
	pauseCancel()
	if err != nil {
		t.Fatalf("pause owner: %v (%s)", err, truncate([]byte(pausedRaw), 400))
	}
	ev.StatePaused = pausedState
	pausedAt := time.Now()
	probe := &cluster.HTTP{Client: &http.Client{Timeout: 3 * time.Second}, ManualKey: fe.env.ManualKey}
	unreachCtx, unreachCancel := context.WithTimeout(ctx, 60*time.Second)
	var lastHealth error
	if err := cluster.Poll(unreachCtx, time.Second, func() (bool, error) {
		lastHealth = probe.Health(unreachCtx, live.HTTPBase())
		return lastHealth != nil, nil
	}); err != nil {
		unreachCancel()
		t.Fatalf("paused owner still answered /health")
	}
	unreachCancel()
	ev.DuringPauseErr = lastHealth.Error()
	if err := ev.Activated(); err != nil {
		t.Fatalf("pause activation: %v", err)
	}

	hold := time.Duration(coreLeaseTTLSeconds+12) * time.Second
	for time.Since(pausedAt) < hold {
		time.Sleep(time.Second)
	}
	ev.HeldSeconds = time.Since(pausedAt).Seconds()
	if !ev.HeldPastLease() {
		t.Fatalf("freeze lasted %.1fs, not past the %.0fs lease", ev.HeldSeconds, coreLeaseTTLSeconds)
	}

	var recovered cluster.Lease
	lastLease := lease
	lastLeaseAt := leaseObservedAt
	var lastProbes []staleSurvivorProbe
	queries, queryErrors, successfulSQLProbes, identifiedSQLProbes, healthySQLProbes := 0, 0, 0, 0, 0
	takeCtx, takeCancel := context.WithTimeout(ctx, 90*time.Second)
	if err := cluster.Poll(takeCtx, time.Second, func() (bool, error) {
		lastProbes = lastProbes[:0]
		var winner *staleSurvivorProbe
		for _, survivor := range survivors {
			probeResult := probeStaleSurvivor(takeCtx, fe, survivor, run.ID)
			lastProbes = append(lastProbes, probeResult)
			queries++
			if probeResult.SQLError != "" {
				queryErrors++
				continue
			}
			successfulSQLProbes++
			if probeResult.RefreshError == "" {
				identifiedSQLProbes++
			}
			if probeResult.HealthError == "" && probeResult.RefreshError == "" {
				healthySQLProbes++
			}
			lastLease = probeResult.Lease
			lastLeaseAt = time.Now().UTC()
			// /health has a separate DB check and can transiently report 503
			// even though this member returned an authoritative lease row.
			// A failed pod refresh, however, leaves the queried IP's pod identity
			// unverified, so it cannot select the owner for the completion probe.
			if probeResult.RefreshError != "" {
				continue
			}
			if probeResult.Lease.Generation <= lease.Generation || probeResult.Lease.OwnerNode == owner.NodeAddress {
				continue
			}
			if winner == nil {
				winner = &probeResult
			}
		}
		if winner != nil {
			recovered = winner.Lease
			leaseBase = fmt.Sprintf("http://%s:%d", winner.IP, cluster.HTTPPort)
			return true, nil
		}
		return false, nil
	}); err != nil {
		takeCancel()
		leaderAddress, leaderError := staleDqliteLeader(fe, survivors)
		var leaderProbe *staleSurvivorProbe
		if leaderAddress != "" {
			leaderMember, ok := fe.topo.ByIP(cluster.HostIP(leaderAddress))
			if !ok {
				for _, observed := range lastProbes {
					if observed.IP == cluster.HostIP(leaderAddress) {
						leaderMember, ok = fe.topo.ByName(observed.Name)
						break
					}
				}
			}
			if ok {
				leaderCtx, leaderCancel := context.WithTimeout(context.Background(), 12*time.Second)
				last := probeStaleSurvivor(leaderCtx, fe, leaderMember, run.ID)
				leaderCancel()
				leaderProbe = &last
			}
		}
		failure := "no lease takeover on identified survivor SQL path"
		if successfulSQLProbes == 0 {
			failure = "product availability failure: no successful survivor SQL path"
		} else if identifiedSQLProbes == 0 {
			failure = "inconclusive: survivor SQL succeeded but pod identity was unavailable"
		}
		t.Fatalf("%s: %v; initial=%+v last=%+v last_at=%s now=%s queries=%d query_errors=%d successful_sql_probes=%d identified_sql_probes=%d healthy_sql_probes=%d survivor_probes=%+v dqlite_leader=%s dqlite_error=%v leader_probe=%+v paused_state=%s held=%.1fs",
			failure, err, lease, lastLease, lastLeaseAt.Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano),
			queries, queryErrors, successfulSQLProbes, identifiedSQLProbes, healthySQLProbes, lastProbes, leaderAddress, leaderError, leaderProbe, pausedState, ev.HeldSeconds)
	}
	takeCancel()
	newOwner, ok := fe.topo.ByNodeAddress(recovered.OwnerNode)
	if !ok {
		newOwner, ok = fe.topo.ByIP(cluster.HostIP(recovered.OwnerNode))
	}
	if !ok {
		t.Fatalf("new owner %s is not a member", recovered.OwnerNode)
	}
	// Lease takeover precedes in-memory recovery and the replacement claim.
	// Wait until the new owner has re-claimed the block and its second external
	// start has reached the held sink, then observe that claim twice before
	// comparing the rejected completion's before/after state. `attempt` is a
	// logical retry counter; ClaimTaskForDispatch increments claim_attempt.
	claimCtx, claimCancel := context.WithTimeout(ctx, 120*time.Second)
	var recoveredBlock cluster.TaskRecipe
	var lastClaimError string
	stableClaimReads := 0
	if err := cluster.Poll(claimCtx, time.Second, func() (bool, error) {
		if len(fe.sink.StartsFor(run.ID, cluster.BlockStep)) <= initialBlockStarts {
			lastClaimError = "second block start not observed"
			stableClaimReads = 0
			return false, nil
		}
		currentLease, lerr := fe.httpAPI.QueryLease(claimCtx, leaseBase, run.ID)
		if lerr != nil {
			lastClaimError = fmt.Sprintf("lease read: %v", lerr)
			stableClaimReads = 0
			return false, nil
		}
		if currentLease.OwnerNode != recovered.OwnerNode || currentLease.Generation != recovered.Generation {
			lastClaimError = fmt.Sprintf("lease changed during recovery: %+v", currentLease)
			stableClaimReads = 0
			return false, nil
		}
		recipes, rerr := fe.httpAPI.QueryTaskRecipes(claimCtx, leaseBase, run.ID)
		if rerr != nil {
			lastClaimError = fmt.Sprintf("task read: %v", rerr)
			stableClaimReads = 0
			return false, nil
		}
		var block cluster.TaskRecipe
		for _, recipe := range recipes {
			if recipe.ID == originalBlock.ID && recipe.TaskID == originalBlock.TaskID {
				block = recipe
				break
			}
		}
		if block.ID == "" || !strings.EqualFold(block.Status, "running") ||
			strings.TrimSpace(block.ClaimedBy) == "" || block.ClaimAttempt <= originalBlock.ClaimAttempt ||
			block.OwnerGeneration != recovered.Generation {
			lastClaimError = fmt.Sprintf("new owner has not re-claimed original block: %+v", block)
			stableClaimReads = 0
			return false, nil
		}
		if block.ID == recoveredBlock.ID && block.Status == recoveredBlock.Status &&
			block.ClaimedBy == recoveredBlock.ClaimedBy && block.ClaimAttempt == recoveredBlock.ClaimAttempt &&
			block.OwnerGeneration == recoveredBlock.OwnerGeneration {
			stableClaimReads++
		} else {
			recoveredBlock = block
			stableClaimReads = 1
		}
		return stableClaimReads >= 2, nil
	}); err != nil {
		claimCancel()
		t.Fatalf("inconclusive: recovered block did not settle behind barrier: %v (last=%s, initial=%+v recovered=%+v starts=%d)",
			err, lastClaimError, originalBlock, recoveredBlock, len(fe.sink.StartsFor(run.ID, cluster.BlockStep)))
	}
	claimCancel()
	if n := len(fe.sink.CompletionsFor(run.ID, cluster.BlockStep)); n != 0 {
		t.Fatalf("block completed %d time(s) before the barrier was released", n)
	}
	if n := len(fe.sink.StartsFor(run.ID, cluster.SuccessorStep)); n != 0 {
		t.Fatalf("successor started %d time(s) before the block barrier was released", n)
	}

	detail, err := fe.httpAPI.GetRun(ctx, leaseBase, job.ID, run.ID)
	if err != nil {
		t.Fatalf("run before stale complete: %v", err)
	}
	before := fingerprintDurableRun(t, ctx, fe, leaseBase, job.ID, run.ID)
	cli := validInternalClient(t, fe, newOwner)
	payload := completePayload(detail, lease, "succeeded", lease.Generation)
	ex := cli.Complete(ctx, cluster.InternalBase(newOwner.IP), payload)
	if ex.Err != "" {
		t.Fatalf("stale complete transport: %v", ex.Err)
	}
	requireHTTPStatus(t, ex.Status, http.StatusConflict, ex.Body)
	code, msg := ParseRefusal(ex.Status, []byte(ex.Body))
	if code != RefusalStaleGeneration {
		t.Fatalf("stale complete code=%q msg=%q, want stale_generation", code, msg)
	}
	after := fingerprintDurableRun(t, ctx, fe, leaseBase, job.ID, run.ID)
	requireNoMutation(t, before, after, "stale-generation complete")

	fe.sink.Release(run.ID)
	recovCtx, recovCancel := context.WithTimeout(ctx, 120*time.Second)
	final := waitRunStatus(t, recovCtx, fe.httpAPI, leaseBase, job.ID, run.ID, "succeeded")
	recovCancel()
	if final.ID != run.ID {
		t.Fatalf("public run id changed %s -> %s", run.ID, final.ID)
	}

	resumeCtx, resumeCancel := context.WithTimeout(ctx, 2*time.Minute)
	resumedState, resumedRaw, err := fe.host.Resume(resumeCtx, live.Node, live.ContainerID)
	resumeCancel()
	resumed = true
	if err != nil {
		t.Fatalf("resume: %v (%s)", err, truncate([]byte(resumedRaw), 400))
	}
	ev.StateResumed = resumedState
	healCtx, healCancel := context.WithTimeout(ctx, 3*time.Minute)
	err = cluster.Poll(healCtx, time.Second, func() (bool, error) {
		return fe.httpAPI.Health(healCtx, live.HTTPBase()) == nil, nil
	})
	healCancel()
	ev.ReachableAfter = err == nil
	if err := ev.Healed(); err != nil {
		t.Fatalf("pause heal: %v", err)
	}
	waitMembership(t, fe, 3*time.Minute)

	writeCoreRecord(t, fe, "stale_generation_complete", map[string]any{
		"run_id":                run.ID,
		"owner_paused":          owner.Name,
		"lease_before":          lease,
		"lease_after":           recovered,
		"lease_queries":         queries,
		"lease_query_errors":    queryErrors,
		"successful_sql_probes": successfulSQLProbes,
		"identified_sql_probes": identifiedSQLProbes,
		"healthy_sql_probes":    healthySQLProbes,
		"recovered_block_claim": recoveredBlock,
		"survivor_probes":       lastProbes,
		"held_seconds":          ev.HeldSeconds,
		"past_lease":            ev.HeldPastLease(),
		"refusal_code":          code,
		"complete_status":       ex.Status,
		"public_status":         final.Status,
		"pause_state_before":    beforeState,
		"pause_state_paused":    pausedState,
	})
}

type staleSurvivorProbe struct {
	Name         string        `json:"name"`
	UID          string        `json:"pod_uid"`
	IP           string        `json:"pod_ip"`
	RefreshError string        `json:"refresh_error,omitempty"`
	HealthError  string        `json:"health_error,omitempty"`
	SQLError     string        `json:"sql_lease_error,omitempty"`
	Lease        cluster.Lease `json:"sql_lease"`
}

func probeStaleSurvivor(ctx context.Context, fe *faultEnv, member cluster.Member, runID string) staleSurvivorProbe {
	result := staleSurvivorProbe{Name: member.Name, UID: member.UID, IP: member.IP}
	refreshCtx, refreshCancel := context.WithTimeout(ctx, 3*time.Second)
	current, err := cluster.RefreshMember(refreshCtx, fe.kube, fe.env.Namespace, member.Name)
	refreshCancel()
	if err != nil {
		result.RefreshError = err.Error()
	} else if current.IP == "" {
		result.RefreshError = "refreshed pod has no IP"
	} else {
		member = current
		result.UID, result.IP = current.UID, current.IP
	}
	if result.IP == "" {
		result.HealthError = "no pod IP"
		result.SQLError = "no pod IP"
		return result
	}
	probe := fe.httpAPI.WithTimeout(3 * time.Second)
	healthCtx, healthCancel := context.WithTimeout(ctx, 3*time.Second)
	if err := probe.Health(healthCtx, member.HTTPBase()); err != nil {
		result.HealthError = err.Error()
	}
	healthCancel()
	queryCtx, queryCancel := context.WithTimeout(ctx, 3*time.Second)
	result.Lease, err = probe.QueryLease(queryCtx, member.HTTPBase(), runID)
	queryCancel()
	if err != nil {
		result.SQLError = err.Error()
	}
	return result
}

func staleDqliteLeader(fe *faultEnv, survivors []cluster.Member) (string, error) {
	var errorsByMember []string
	for _, survivor := range survivors {
		refreshCtx, refreshCancel := context.WithTimeout(context.Background(), 3*time.Second)
		current, refreshErr := cluster.RefreshMember(refreshCtx, fe.kube, fe.env.Namespace, survivor.Name)
		refreshCancel()
		if refreshErr != nil || current.IP == "" {
			refreshMessage := "no pod IP"
			if refreshErr != nil {
				refreshMessage = refreshErr.Error()
			}
			errorsByMember = append(errorsByMember, fmt.Sprintf("%s/%s/%s: refresh %s", survivor.Name, survivor.UID, survivor.IP, refreshMessage))
			continue
		}
		queryCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		leader, _, err := cluster.QueryNode(queryCtx, current.DqliteAddr())
		cancel()
		if err == nil && leader != nil && leader.Address != "" {
			return leader.Address, nil
		}
		errorsByMember = append(errorsByMember, fmt.Sprintf("%s/%s/%s: %v", current.Name, current.UID, current.IP, err))
	}
	return "", fmt.Errorf("dqlite leader unavailable from survivors: %s", strings.Join(errorsByMember, "; "))
}

func runCommitBeforeResponseLoss(t *testing.T, fe *faultEnv) {
	refreshTopo(t, fe)
	ctx := context.Background()
	member := fe.leader
	survivor := otherMember(fe.topo, member)
	in, err := faults.NewInterposer(member.HTTPBase())
	if err != nil {
		t.Fatalf("interposer: %v", err)
	}
	if err := in.Start(); err != nil {
		t.Fatalf("interposer listen: %v", err)
	}
	t.Cleanup(func() { _ = in.Close(context.Background()) })

	job, alias := applyFaultFixture(t, fe, member, "loss-core", 2)
	in.SetPolicy(faults.Policy{Mode: faults.ModeDropResponse, Method: http.MethodPost, PathContains: "/run"})
	before := len(in.Operations())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, in.BaseURL()+"/v1/jobs/"+job.ID+"/run", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Caesium-Manual-Trigger-Key", fe.env.ManualKey)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, clientErr := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if clientErr == nil {
		t.Fatal("client received a usable response although the drop fault was armed")
	}

	var op faults.Operation
	deadline := time.Now().Add(60 * time.Second)
	for {
		ops := in.Operations()
		if len(ops) > before && !ops[len(ops)-1].SettledAt.IsZero() {
			op = ops[len(ops)-1]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("interposer never settled: %s", jsonString(in.Operations()))
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !op.PossiblyCommitted || op.UpstreamStatus != http.StatusAccepted {
		t.Fatalf("lost response was not a committed 202: %+v", op)
	}
	var acked cluster.Run
	if err := json.Unmarshal([]byte(op.UpstreamBody), &acked); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if _, err := uuid.Parse(acked.ID); err != nil {
		t.Fatalf("upstream 202 run id is not a uuid: %q", acked.ID)
	}
	reconCtx, reconCancel := context.WithTimeout(ctx, 2*time.Minute)
	var reconciled cluster.Run
	if err := cluster.Poll(reconCtx, time.Second, func() (bool, error) {
		got, gerr := fe.httpAPI.GetRun(reconCtx, survivor.HTTPBase(), job.ID, acked.ID)
		if gerr != nil {
			return false, nil
		}
		reconciled = got
		return true, nil
	}); err != nil {
		reconCancel()
		t.Fatalf("possibly-committed run not reconcilable: %v", err)
	}
	reconCancel()
	termCtx, termCancel := context.WithTimeout(ctx, 5*time.Minute)
	final := waitRunTerminal(t, termCtx, fe.httpAPI, survivor.HTTPBase(), job.ID, acked.ID)
	termCancel()

	in.SetPolicy(faults.Policy{Mode: faults.ModePassThrough})
	healOp, healRun, healErr := probeInterposerHealed(ctx, fe, in, job.ID, 30*time.Second)
	if healErr != nil {
		t.Fatalf("interposer did not heal: %v", healErr)
	}

	writeCoreRecord(t, fe, "commit_before_response_loss", map[string]any{
		"alias":           alias,
		"client_error":    clientErr.Error(),
		"upstream_status": op.UpstreamStatus,
		"reconciled_run":  reconciled.ID,
		"final_status":    final.Status,
		"outcome":         OutcomePossiblyCommitted,
		"heal_run":        healRun.ID,
		"heal_status":     healOp.UpstreamStatus,
	})
}

func runQuorumLoss(t *testing.T, fe *faultEnv) {
	refreshTopo(t, fe)
	ctx := context.Background()
	minority := otherMember(fe.topo, fe.leader)
	iso := isolateMember(t, fe, minority)

	majority := iso.majority[0]
	job, alias := applyFaultFixture(t, fe, majority, "quorum-maj", 2)
	majRun, _, err := fe.httpAPI.TriggerRun(ctx, majority.HTTPBase(), job.ID)
	if err != nil {
		t.Fatalf("majority trigger: %v", err)
	}
	requireSplitActive(t, fe, iso)

	type attempt struct {
		Deadline time.Duration
		Status   int
		Body     string
		Err      string
		Outcome  string
		RunID    string
	}
	var attempts []attempt
	minJob, _ := applyFaultFixture(t, fe, majority, "quorum-min", 2)
	for _, d := range []time.Duration{5 * time.Second, 15 * time.Second} {
		client := fe.httpAPI.WithTimeout(d)
		reqCtx, cancel := context.WithTimeout(ctx, d+2*time.Second)
		status, raw, terr := client.TriggerRunRaw(reqCtx, minority.HTTPBase(), minJob.ID)
		cancel()
		a := attempt{Deadline: d, Status: status, Body: string(raw)}
		if terr != nil {
			a.Err = terr.Error()
		}
		a.Outcome = ClassifyQuorumLossMutation(status, raw, terr)
		if a.Outcome == OutcomeAccepted {
			t.Fatalf("minority mutation was accepted while the 2-1 split was proven active (deadline %s run %s)",
				d, runIDFromBody(raw))
		}
		a.RunID = runIDFromBody(raw)
		attempts = append(attempts, a)
		t.Logf("minority mutation deadline=%s outcome=%s status=%d err=%q", d, a.Outcome, status, a.Err)
	}

	majCtx, majCancel := context.WithTimeout(ctx, 5*time.Minute)
	majFinal := waitRunStatus(t, majCtx, fe.httpAPI, majority.HTTPBase(), job.ID, majRun.ID, "succeeded")
	majCancel()

	iso.heal(t, fe)
	waitMembership(t, fe, 3*time.Minute)

	var reconciled []map[string]any
	for _, a := range attempts {
		entry := map[string]any{"deadline": a.Deadline.String(), "outcome": a.Outcome, "status": a.Status}
		if a.RunID != "" {
			got, gerr := fe.httpAPI.GetRun(ctx, majority.HTTPBase(), minJob.ID, a.RunID)
			if gerr != nil {
				t.Fatalf("accepted/identified minority run %s missing after heal: %v", a.RunID, gerr)
			}
			entry["present"] = true
			entry["status_after_heal"] = got.Status
		} else {
			entry["identity"] = "unknown_possibly_committed"
		}
		reconciled = append(reconciled, entry)
	}

	writeCoreRecord(t, fe, "quorum_loss_uncertain_write", map[string]any{
		"minority":          minority.Name,
		"majority":          majority.Name,
		"majority_run":      majRun.ID,
		"majority_final":    majFinal.Status,
		"majority_alias":    alias,
		"attempts":          attempts,
		"reconciled":        reconciled,
		"fault_plans":       len(iso.plans),
		"timeout_is_reject": false,
	})
}

func runDurableEventCrash(t *testing.T, fe *faultEnv) {
	if !instrumented() {
		t.Skip("release image: durable-event-before-delivery crash needs the instrumented image. " +
			"scripts/robustness.sh requires this subtest only when CAESIUM_ROBUSTNESS_INSTRUMENTED_IMAGE is set, " +
			"so this skip cannot stand in for the instrumented result.")
	}
	refreshTopo(t, fe)
	ctx := context.Background()
	dir := fe.testdir
	live := make([]cluster.Member, 0, len(fe.topo.Members))
	for _, m := range fe.topo.Members {
		refreshed, err := cluster.RefreshMember(ctx, fe.kube, fe.env.Namespace, m.Name)
		if err != nil || refreshed.ContainerID == "" {
			t.Fatalf("refresh %s: %v", m.Name, err)
		}
		live = append(live, refreshed)
	}
	directive := `{"armed":true,"types":["task_started"],"max_hold_ms":120000}`
	for _, m := range live {
		armCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		ev, err := fe.host.ArmBusPublishPause(armCtx, m.Node, m.ContainerID, dir, directive)
		cancel()
		if err != nil {
			t.Fatalf("arm %s: %v (%s)", m.Name, err, truncate([]byte(ev), 400))
		}
	}
	disarmed := false
	t.Cleanup(func() {
		if disarmed {
			return
		}
		for _, m := range live {
			cleanCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			_, _ = fe.host.DisarmBusPublishPause(cleanCtx, m.Node, m.ContainerID, dir)
			cancel()
		}
	})

	member := fe.leader
	job, _ := applyFaultFixture(t, fe, member, "buskill", 2)
	run, _, err := fe.httpAPI.TriggerRun(ctx, member.HTTPBase(), job.ID)
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}

	var hookEvidence []faults.HookEvidence
	var held uint64
	var publisher cluster.Member
	hookCtx, hookCancel := context.WithTimeout(ctx, 3*time.Minute)
	err = cluster.Poll(hookCtx, 2*time.Second, func() (bool, error) {
		hookEvidence = nil
		held = 0
		for i, m := range live {
			raw, rerr := fe.host.BusPublishHookLog(hookCtx, m.Node, m.ContainerID, dir)
			if rerr != nil {
				return false, nil
			}
			entries, perr := faults.ParseHookLog(raw)
			if perr != nil {
				return false, fmt.Errorf("inconclusive: %s hook log: %w", m.Name, perr)
			}
			ev := faults.SummariseHookLog(m.Name, run.ID, entries)
			if len(ev.Entered) > 0 {
				hookEvidence = append(hookEvidence, ev)
				if seqs := ev.HeldSequences(); len(seqs) > 0 {
					held = seqs[0]
					publisher = live[i]
				}
			}
		}
		return held != 0, nil
	})
	hookCancel()
	if err != nil || publisher.Name == "" {
		t.Fatalf("no publisher entered the hook for run %s: %v", run.ID, err)
	}
	requireFaultActivated(t, held != 0, "hook_entered_before_publish")

	rowCtx, rowCancel := context.WithTimeout(ctx, 60*time.Second)
	rows, _, err := readPersistedEvents(rowCtx, fe.httpAPI, member.HTTPBase(), run.ID, 2000)
	rowCancel()
	if err != nil {
		t.Fatalf("inconclusive: persisted events: %v", err)
	}
	row, ok := findPersisted(rows, held)
	if !ok || !row.BusPending || row.DispatchedAt != "" {
		t.Fatalf("held event %d is not a committed pending row: %+v", held, row)
	}

	killCtx, killCancel := context.WithTimeout(ctx, 60*time.Second)
	killAck, err := cluster.RequestHost(killCtx, fe.kube, fe.env.Namespace, cluster.HostRequest{
		RequestID: uuid.NewString(), Action: cluster.ActionKill,
		OwnerPod: publisher.Name, OwnerKindNode: publisher.Node,
		OwnerContainerID: publisher.ContainerID, RunID: run.ID,
	})
	killCancel()
	if err != nil {
		t.Fatalf("kill publisher: %v", err)
	}
	if strings.TrimSpace(killAck.Evidence) == "" {
		t.Fatal("absent kill evidence")
	}

	for _, m := range live {
		if m.Name == publisher.Name {
			continue
		}
		dctx, dcancel := context.WithTimeout(ctx, 60*time.Second)
		if _, err := fe.host.DisarmBusPublishPause(dctx, m.Node, m.ContainerID, dir); err != nil {
			t.Logf("disarm survivor %s: %v", m.Name, err)
		}
		dcancel()
	}
	disarmed = true

	survivor := otherMember(fe.topo, publisher)
	markCtx, markCancel := context.WithTimeout(ctx, 3*time.Minute)
	var after persistedEvent
	err = cluster.Poll(markCtx, 2*time.Second, func() (bool, error) {
		rows, _, rerr := readPersistedEvents(markCtx, fe.httpAPI, survivor.HTTPBase(), run.ID, 2000)
		if rerr != nil {
			return false, nil
		}
		row, ok := findPersisted(rows, held)
		if !ok {
			return false, nil
		}
		after = row
		return !row.BusPending, nil
	})
	markCancel()
	if err != nil {
		t.Fatalf("durable event %d was not replayed after publisher kill: %v", held, err)
	}

	restartCtx, restartCancel := context.WithTimeout(ctx, 90*time.Second)
	if _, err := cluster.RequestHost(restartCtx, fe.kube, fe.env.Namespace, cluster.HostRequest{
		RequestID: uuid.NewString(), Action: cluster.ActionRestart,
		OwnerPod: publisher.Name, OwnerKindNode: publisher.Node,
	}); err != nil {
		t.Logf("restart publisher: %v", err)
	}
	restartCancel()
	waitMembership(t, fe, 3*time.Minute)

	writeCoreRecord(t, fe, "durable_event_before_delivery_crash", map[string]any{
		"run_id":         run.ID,
		"held_sequence":  held,
		"publisher":      publisher.Name,
		"row_while_held": row,
		"row_after_kill": after,
		"kill_evidence":  truncate([]byte(killAck.Evidence), 1500),
		"hook_members":   len(hookEvidence),
	})
}
