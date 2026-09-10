//go:build integration

package robustness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/test/robustness/cluster"
	"github.com/caesium-cloud/caesium/test/robustness/recorder"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

func TestOwnerCrash(t *testing.T) {
	ctx := context.Background()
	env, err := cluster.LoadEnv()
	if err != nil {
		t.Fatalf("environment: %v", err)
	}
	kube, err := cluster.InClusterClient()
	if err != nil {
		t.Fatalf("kube client: %v", err)
	}
	sink := recorder.New()
	if err := sink.Start(); err != nil {
		t.Fatalf("recorder listen %s: %v", recorder.ListenAddr, err)
	}
	t.Cleanup(func() {
		_ = sink.Close(context.Background())
	})
	if err := cluster.EnsureRecorderService(ctx, kube, env.Namespace); err != nil {
		t.Fatalf("recorder service: %v", err)
	}

	httpAPI := cluster.NewHTTP(env.ManualKey)
	topo := mustReadyTopology(t, ctx, kube, env)
	for _, m := range topo.Members {
		if err := httpAPI.Health(ctx, m.HTTPBase()); err != nil {
			t.Fatalf("pod %s /health: %v", m.Name, err)
		}
	}
	addrs := dqliteAddresses(topo)
	memberCtx, memberCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer memberCancel()
	membership, err := cluster.WaitMembership(memberCtx, addrs)
	if err != nil {
		t.Fatalf("dqlite membership: %v", err)
	}
	t.Logf("dqlite leader=%s members=%d", membership.Leader.Address, len(membership.Members))
	if raw, err := httpAPI.SystemNodes(ctx, topo.Members[0].HTTPBase()); err != nil {
		t.Logf("GET /v1/system/nodes diagnostic: %v", err)
	} else {
		t.Logf("GET /v1/system/nodes diagnostic: %s", raw)
	}

	probeID := "probe-" + uuid.NewString()
	probeAlias := "robustness-probe-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	if err := httpAPI.Apply(ctx, topo.Members[0].HTTPBase(), []jobdef.Definition{cluster.ProbeDefinition(probeAlias, probeID, env.TaskImage)}); err != nil {
		t.Fatalf("apply probe job: %v", err)
	}
	probeJob, err := httpAPI.JobByAlias(ctx, topo.Members[0].HTTPBase(), probeAlias)
	if err != nil {
		t.Fatalf("read probe job after apply: %v", err)
	}
	if _, _, err := httpAPI.TriggerRun(ctx, topo.Members[0].HTTPBase(), probeJob.ID); err != nil {
		t.Fatalf("trigger probe job: %v", err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := cluster.WaitProbe(probeCtx, sink, probeID); err != nil {
		t.Fatalf("task/sink connectivity probe missing (inconclusive): %v", err)
	}
	t.Logf("connectivity probe %s observed", probeID)

	t.Run("owner_is_leader", func(t *testing.T) {
		runOwnerCrash(t, kube, httpAPI, sink, env, true)
	})
	t.Run("owner_is_not_leader", func(t *testing.T) {
		runOwnerCrash(t, kube, httpAPI, sink, env, false)
	})

	doneCtx, doneCancel := context.WithTimeout(ctx, 30*time.Second)
	defer doneCancel()
	if _, err := cluster.RequestHost(doneCtx, kube, env.Namespace, cluster.HostRequest{
		RequestID: uuid.NewString(),
		Action:    cluster.ActionDone,
	}); err != nil {
		t.Logf("host done: %v", err)
	}
	if err := cluster.WriteRecords(ctx, kube, env.Namespace, "events", sink.Events()); err != nil {
		t.Logf("persist recorder events: %v (missing recorder data is inconclusive)", err)
	}
}

func runOwnerCrash(t *testing.T, kube *kubernetes.Clientset, httpAPI *cluster.HTTP, sink *recorder.Sink, env cluster.Env, wantLeader bool) {
	ctx := context.Background()
	name := "owner_is_not_leader"
	if wantLeader {
		name = "owner_is_leader"
	}

	readyCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := cluster.Poll(readyCtx, time.Second, func() (bool, error) {
		_, err := cluster.RequireReadyTopology(readyCtx, kube, env.Namespace, env.CandidateDigest)
		return err == nil, nil
	}); err != nil {
		t.Fatalf("cluster not ready for %s: %v", name, err)
	}
	topo := mustReadyTopology(t, ctx, kube, env)
	memberCtx, memberCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer memberCancel()
	membership, err := cluster.WaitMembership(memberCtx, dqliteAddresses(topo))
	if err != nil {
		t.Fatalf("dqlite membership before %s: %v", name, err)
	}
	leader, ok := topo.ByIP(cluster.HostIP(membership.Leader.Address))
	if !ok {
		t.Fatalf("dqlite leader %s is not a caesium pod", membership.Leader.Address)
	}

	var owner cluster.Member
	if wantLeader {
		owner = leader
	} else {
		survivors := topo.Survivors(leader)
		if len(survivors) == 0 {
			t.Fatalf("no non-leader member")
		}
		owner = survivors[0]
	}
	if cluster.IsControlPlaneNode(owner.Node) {
		t.Fatalf("owner pod %s is on control-plane node %s", owner.Name, owner.Node)
	}

	alias := fmt.Sprintf("robustness-%s-%s", strings.ReplaceAll(name, "_", "-"), strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
	def := cluster.FixtureDefinition(alias, env.TaskImage)
	if err := def.Validate(); err != nil {
		t.Fatalf("fixture invalid: %v", err)
	}
	if err := httpAPI.Apply(ctx, owner.HTTPBase(), []jobdef.Definition{def}); err != nil {
		t.Fatalf("apply fixture: %v", err)
	}
	job, err := httpAPI.JobByAlias(ctx, owner.HTTPBase(), alias)
	if err != nil {
		t.Fatalf("read applied job: %v", err)
	}

	cordonCtx, cordonCancel := context.WithTimeout(ctx, 60*time.Second)
	defer cordonCancel()
	cordonAck, err := cluster.RequestHost(cordonCtx, kube, env.Namespace, cluster.HostRequest{
		RequestID:     uuid.NewString(),
		Action:        cluster.ActionCordon,
		OwnerPod:      owner.Name,
		OwnerKindNode: owner.Node,
	})
	if err != nil {
		t.Fatalf("cordon %s: %v", owner.Node, err)
	}
	t.Logf("cordon ack: %s", cordonAck.Evidence)

	triggeredAt := time.Now().UTC()
	run, raw, err := httpAPI.TriggerRun(ctx, owner.HTTPBase(), job.ID)
	if err != nil {
		t.Fatalf("DT-ADMIT-01 trigger against %s: %v", owner.HTTPBase(), err)
	}
	t.Logf("admitted run %s via %s body=%s", run.ID, owner.Name, truncate(raw, 256))

	survivors := topo.Survivors(owner)
	if len(survivors) == 0 {
		t.Fatalf("no survivor to query lease")
	}
	leaseBase := survivors[0].HTTPBase()
	leaseCtx, leaseCancel := context.WithTimeout(ctx, 90*time.Second)
	defer leaseCancel()
	lease, err := cluster.WaitLease(leaseCtx, httpAPI, leaseBase, run.ID, owner.NodeAddress)
	if err != nil {
		t.Fatalf("lease before kill: %v", err)
	}
	t.Logf("lease owner_node=%s generation=%d expires=%s", lease.OwnerNode, lease.Generation, lease.LeaseExpiresAt)

	startCtx, startCancel := context.WithTimeout(ctx, 90*time.Second)
	defer startCancel()
	if err := cluster.Poll(startCtx, 500*time.Millisecond, func() (bool, error) {
		return len(sink.StartsFor(run.ID, cluster.BlockStep)) > 0, nil
	}); err != nil {
		t.Fatalf("blocked-effect start missing before kill (inconclusive): %v events=%s", err, eventsJSON(sink))
	}

	placeCtx, placeCancel := context.WithTimeout(ctx, 60*time.Second)
	defer placeCancel()
	if err := cluster.Poll(placeCtx, time.Second, func() (bool, error) {
		pods, err := cluster.ListTaskPods(placeCtx, kube, env.Namespace)
		if err != nil {
			return false, nil
		}
		var fixture []corev1.Pod
		for _, p := range pods {
			if p.CreationTimestamp.Time.Before(triggeredAt.Add(-5 * time.Second)) {
				continue
			}
			if strings.Contains(strings.ToLower(p.Name), "probe") {
				continue
			}
			fixture = append(fixture, p)
		}
		if len(fixture) == 0 {
			return false, nil
		}
		var bound []corev1.Pod
		for _, p := range fixture {
			if p.Spec.NodeName == "" {
				return false, nil
			}
			bound = append(bound, p)
		}
		if err := cluster.RequireTasksOnSurvivors(bound, owner); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		t.Fatalf("fixture task placement: %v", err)
	}

	refreshCtx, refreshCancel := context.WithTimeout(ctx, 30*time.Second)
	defer refreshCancel()
	refreshed, err := cluster.DiscoverMembership(refreshCtx, dqliteAddresses(topo))
	if err != nil {
		t.Fatalf("refresh leader before kill: %v", err)
	}
	leaderNow, ok := topo.ByIP(cluster.HostIP(refreshed.Leader.Address))
	if !ok {
		t.Fatalf("refreshed leader %s is not a caesium pod", refreshed.Leader.Address)
	}
	ownerIsLeaderNow := owner.Name == leaderNow.Name
	if ownerIsLeaderNow != wantLeader {
		t.Fatalf("invalidated %s: owner/leader relationship changed (owner=%s leader=%s)", name, owner.Name, leaderNow.Name)
	}
	liveOwner, err := cluster.RefreshMember(ctx, kube, env.Namespace, owner.Name)
	if err != nil {
		t.Fatalf("refresh owner container: %v", err)
	}
	if liveOwner.ContainerID == "" {
		t.Fatalf("owner %s missing container id", owner.Name)
	}

	killCtx, killCancel := context.WithTimeout(ctx, 60*time.Second)
	defer killCancel()
	killAck, err := cluster.RequestHost(killCtx, kube, env.Namespace, cluster.HostRequest{
		RequestID:        uuid.NewString(),
		Action:           cluster.ActionKill,
		OwnerPod:         owner.Name,
		OwnerKindNode:    owner.Node,
		OwnerContainerID: liveOwner.ContainerID,
		RunID:            run.ID,
	})
	if err != nil {
		t.Fatalf("kill owner: %v", err)
	}
	if strings.TrimSpace(killAck.Evidence) == "" {
		t.Fatalf("absent kill evidence")
	}
	if !killEvidenceShowsDeath(killAck.Evidence, liveOwner.ContainerID) {
		t.Fatalf("kill evidence did not show process exit/stopped task for %s:\n%s", liveOwner.ContainerID, killAck.Evidence)
	}
	t.Logf("kill evidence accepted for %s on %s", liveOwner.ContainerID, owner.Node)

	sink.Release(run.ID)
	recoveryStart := time.Now()
	recoverCtx, recoverCancel := context.WithTimeout(ctx, 120*time.Second)
	defer recoverCancel()

	var final cluster.Run
	var recovered cluster.Lease
	if err := cluster.Poll(recoverCtx, time.Second, func() (bool, error) {
		got, err := httpAPI.QueryLease(recoverCtx, leaseBase, run.ID)
		if err != nil {
			return false, nil
		}
		if got.Generation <= lease.Generation {
			return false, nil
		}
		if got.OwnerNode == owner.NodeAddress {
			return false, nil
		}
		recovered = got
		detail, err := httpAPI.GetRun(recoverCtx, leaseBase, job.ID, run.ID)
		if err != nil {
			return false, nil
		}
		if !strings.EqualFold(detail.Status, "succeeded") {
			return false, nil
		}
		final = detail
		return true, nil
	}); err != nil {
		t.Fatalf("DT-RECOVER-01 takeover/completion while kubelet stopped: %v lease=%+v events=%s", err, recovered, eventsJSON(sink))
	}
	measured := time.Since(recoveryStart)
	t.Logf("DT-RECOVER-01 watchdog_s=120 measured_s=%.3f generation %d -> %d owner %s -> %s",
		measured.Seconds(), lease.Generation, recovered.Generation, lease.OwnerNode, recovered.OwnerNode)
	if measured > 120*time.Second {
		t.Fatalf("recovery exceeded 120s watchdog: %s", measured)
	}
	if recovered.OwnerNode == owner.NodeAddress {
		t.Fatalf("surviving owner was not recorded")
	}

	blockStarts := sink.StartsFor(run.ID, cluster.BlockStep)
	blockDone := sink.CompletionsFor(run.ID, cluster.BlockStep)
	succStarts := sink.StartsFor(run.ID, cluster.SuccessorStep)
	succDone := sink.CompletionsFor(run.ID, cluster.SuccessorStep)
	if len(blockStarts) == 0 || len(blockDone) == 0 {
		t.Fatalf("recorder missing block start/complete (inconclusive): starts=%d completes=%d", len(blockStarts), len(blockDone))
	}
	if len(succStarts) == 0 || len(succDone) == 0 {
		t.Fatalf("legal successor did not execute: starts=%d completes=%d", len(succStarts), len(succDone))
	}
	if len(blockStarts) > 1 {
		t.Logf("duplicate block attempts retained: %d", len(blockStarts))
	}
	if !strings.EqualFold(final.Status, "succeeded") {
		t.Fatalf("public run status %s, want succeeded", final.Status)
	}
	if final.ID != run.ID {
		t.Fatalf("public run id changed from %s to %s", run.ID, final.ID)
	}
	catalog, err := httpAPI.ListJobTasks(ctx, leaseBase, job.ID)
	if err != nil {
		t.Fatalf("list job tasks for nonce correlation: %v", err)
	}
	nameByTaskID := map[string]string{}
	for _, ct := range catalog {
		if ct.ID != "" && ct.Name != "" {
			nameByTaskID[ct.ID] = ct.Name
		}
	}
	correlateSink(t, final, nameByTaskID, blockStarts, cluster.BlockStep)
	correlateSink(t, final, nameByTaskID, succStarts, cluster.SuccessorStep)
	correlateSink(t, final, nameByTaskID, blockDone, cluster.BlockStep)
	correlateSink(t, final, nameByTaskID, succDone, cluster.SuccessorStep)

	restartCtx, restartCancel := context.WithTimeout(ctx, 60*time.Second)
	defer restartCancel()
	restartAck, err := cluster.RequestHost(restartCtx, kube, env.Namespace, cluster.HostRequest{
		RequestID:     uuid.NewString(),
		Action:        cluster.ActionRestart,
		OwnerPod:      owner.Name,
		OwnerKindNode: owner.Node,
	})
	if err != nil {
		t.Fatalf("restart/rejoin: %v", err)
	}
	t.Logf("restart ack: %s", restartAck.Evidence)

	rejoinCtx, rejoinCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer rejoinCancel()
	if err := cluster.Poll(rejoinCtx, time.Second, func() (bool, error) {
		back, err := cluster.RefreshMember(rejoinCtx, kube, env.Namespace, owner.Name)
		if err != nil {
			return false, nil
		}
		if back.UID != owner.UID {
			return false, fmt.Errorf("pod UID changed on rejoin: %s -> %s", owner.UID, back.UID)
		}
		if back.VolumeName != "" && owner.VolumeName != "" && back.VolumeName != owner.VolumeName {
			return false, fmt.Errorf("PVC volume changed: %s -> %s", owner.VolumeName, back.VolumeName)
		}
		if back.IP != "" && owner.IP != "" && back.IP != owner.IP {
			return false, fmt.Errorf("pod IP changed: %s -> %s (sandbox not retained)", owner.IP, back.IP)
		}
		if !cluster.PodReady(&back.Pod) {
			return false, nil
		}
		topoNow, err := cluster.RequireReadyTopology(rejoinCtx, kube, env.Namespace, env.CandidateDigest)
		if err != nil {
			return false, nil
		}
		_, err = cluster.DiscoverMembership(rejoinCtx, dqliteAddresses(topoNow))
		return err == nil, nil
	}); err != nil {
		t.Fatalf("old member failed to restart/rejoin with retained volume/address: %v", err)
	}

	if err := cluster.WriteRecords(ctx, kube, env.Namespace, name, map[string]any{
		"subtest":            name,
		"run_id":             run.ID,
		"owner_pod":          owner.Name,
		"owner_node":         owner.Node,
		"owner_address":      owner.NodeAddress,
		"leader_before":      leader.Name,
		"lease_before":       lease,
		"lease_after":        recovered,
		"recovery_seconds":   measured.Seconds(),
		"block_starts":       len(blockStarts),
		"block_completions":  len(blockDone),
		"successor_starts":   len(succStarts),
		"successor_complete": len(succDone),
		"public_status":      final.Status,
		"kill_evidence":      killAck.Evidence,
	}); err != nil {
		t.Logf("persist %s records: %v (missing recorder data is inconclusive)", name, err)
	}
}

func correlateSink(t *testing.T, run cluster.Run, nameByTaskID map[string]string, events []recorder.Event, step string) {
	t.Helper()
	if len(events) == 0 {
		t.Fatalf("no sink events to correlate for step %s", step)
	}
	var matching []cluster.Task
	for _, tr := range run.Tasks {
		if nameByTaskID[tr.TaskID] == step {
			matching = append(matching, tr)
		}
	}
	if len(matching) == 0 {
		t.Fatalf("sink step %s has no public task-run identity (catalog names=%v public tasks=%d)", step, nameByTaskID, len(run.Tasks))
	}
	ids := make([]string, 0, len(matching))
	for _, tr := range matching {
		ids = append(ids, fmt.Sprintf("%s/attempt=%d/status=%s", tr.ID, tr.Attempt, tr.Status))
	}
	seenNonce := map[string]struct{}{}
	for _, ev := range events {
		if strings.TrimSpace(ev.Nonce) == "" {
			t.Fatalf("sink %s event missing nonce", step)
		}
		if ev.Step != "" && ev.Step != step {
			t.Fatalf("sink event step %s != %s", ev.Step, step)
		}
		if ev.RunID != run.ID {
			t.Fatalf("sink nonce %s run_id %s != public run %s", ev.Nonce, ev.RunID, run.ID)
		}
		seenNonce[ev.Nonce] = struct{}{}
		t.Logf("correlated nonce=%s step=%s with public task-runs %s", ev.Nonce, step, strings.Join(ids, ","))
	}
	if len(seenNonce) != len(events) {
		t.Logf("duplicate sink nonces for step %s retained (%d events, %d unique)", step, len(events), len(seenNonce))
	}
}

func mustReadyTopology(t *testing.T, ctx context.Context, kube *kubernetes.Clientset, env cluster.Env) cluster.Topology {
	t.Helper()
	wait, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var topo cluster.Topology
	if err := cluster.Poll(wait, time.Second, func() (bool, error) {
		got, err := cluster.RequireReadyTopology(wait, kube, env.Namespace, env.CandidateDigest)
		if err != nil {
			return false, nil
		}
		topo = got
		return true, nil
	}); err != nil {
		t.Fatalf("ready topology: %v", err)
	}
	return topo
}

func dqliteAddresses(topo cluster.Topology) []string {
	out := make([]string, 0, len(topo.Members))
	for _, m := range topo.Members {
		out = append(out, m.DqliteAddr())
	}
	return out
}

func killEvidenceShowsDeath(evidence, containerID string) bool {
	if strings.TrimSpace(evidence) == "" || containerID == "" {
		return false
	}
	short := containerID
	if len(short) > 12 {
		short = short[:12]
	}
	if !strings.Contains(evidence, short) {
		return false
	}
	var listingHasCID, listingRunning, listingStopped bool
	for _, line := range strings.Split(evidence, "\n") {
		trim := strings.TrimSpace(line)
		lower := strings.ToLower(trim)
		if strings.HasPrefix(lower, "kubelet stopped") || strings.HasPrefix(lower, "ctr kill") {
			continue
		}
		if !strings.Contains(trim, short) && !strings.Contains(trim, containerID) {
			continue
		}
		listingHasCID = true
		if strings.Contains(lower, "running") {
			listingRunning = true
		}
		if strings.Contains(lower, "stopped") || strings.Contains(lower, "exited") || strings.Contains(lower, "killed") {
			listingStopped = true
		}
	}
	if listingRunning {
		return false
	}
	if listingStopped {
		return true
	}
	if listingHasCID {
		return false
	}
	// CID is in the kill command line but gone from ctr tasks list.
	return true
}

func eventsJSON(sink *recorder.Sink) string {
	b, _ := json.Marshal(sink.Events())
	return string(b)
}

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
