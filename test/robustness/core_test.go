//go:build integration

package robustness

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/test/robustness/cluster"
	"github.com/caesium-cloud/caesium/test/robustness/faults"
	"github.com/caesium-cloud/caesium/test/robustness/recorder"
	"github.com/google/uuid"
)

// TestCore is B3's fenced recovery, dispatch, and authorization suite.
// Each subtest either activates its fault and records independent evidence
// or fails closed. A skip is never a pass for a required subtest; the
// instrumented durable-event crash is required only when the harness
// deploys the testfault image.
func TestCore(t *testing.T) {
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
	t.Cleanup(func() { _ = sink.Close(context.Background()) })
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
	memberCtx, memberCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer memberCancel()
	membership, err := cluster.WaitMembership(memberCtx, dqliteAddresses(topo))
	if err != nil {
		t.Fatalf("dqlite membership: %v", err)
	}
	leader, ok := topo.ByIP(cluster.HostIP(membership.Leader.Address))
	if !ok {
		t.Fatalf("dqlite leader %s is not a caesium pod", membership.Leader.Address)
	}
	t.Logf("dqlite leader=%s members=%d", membership.Leader.Address, len(membership.Members))

	probeID := "core-probe-" + uuid.NewString()
	probeAlias := "core-probe-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	if err := httpAPI.Apply(ctx, leader.HTTPBase(), []jobdef.Definition{cluster.ProbeDefinition(probeAlias, probeID, env.TaskImage)}); err != nil {
		t.Fatalf("apply probe job: %v", err)
	}
	probeJob, err := httpAPI.JobByAlias(ctx, leader.HTTPBase(), probeAlias)
	if err != nil {
		t.Fatalf("read probe job: %v", err)
	}
	if _, _, err := httpAPI.TriggerRun(ctx, leader.HTTPBase(), probeJob.ID); err != nil {
		t.Fatalf("trigger probe job: %v", err)
	}
	probeCtx, probeCancel := context.WithTimeout(ctx, 3*time.Minute)
	defer probeCancel()
	if err := cluster.WaitProbe(probeCtx, sink, probeID); err != nil {
		t.Fatalf("task/sink connectivity probe missing (inconclusive): %v", err)
	}

	fe := &faultEnv{
		env:     env,
		kube:    kube,
		sink:    sink,
		httpAPI: httpAPI,
		topo:    topo,
		leader:  leader,
		host:    faults.HostController{Kube: kube, Namespace: env.Namespace},
		testdir: firstNonEmptyStr(os.Getenv("CAESIUM_ROBUSTNESS_TESTFAULT_DIR"), "/tmp/caesium-testfault"),
	}

	writeCoreRecord(t, fe, "core_topology", map[string]any{
		"leader":            leader.Name,
		"members":           len(topo.Members),
		"candidate_digest":  env.CandidateDigest,
		"candidate_sha":     env.CandidateSHA,
		"instrumented":      instrumented(),
		"membership_leader": membership.Leader.Address,
	})

	// Least-disruptive cases first. Pause/partition/kill run later so a
	// leftover freeze cannot starve the authorization and recipe checks.
	t.Run("terminal_no_regress", func(t *testing.T) { runTerminalNoRegress(t, fe) })
	t.Run("frozen_retry_recipe", func(t *testing.T) { runFrozenRetry(t, fe) })
	t.Run("fan_in_predecessors", func(t *testing.T) { runFanIn(t, fe) })
	t.Run("wrong_token_internal", func(t *testing.T) { runWrongToken(t, fe) })
	t.Run("invalid_mtls_peer", func(t *testing.T) { runInvalidMTLS(t, fe) })
	t.Run("cancel_completion_race", func(t *testing.T) { runCancelCompletionRace(t, fe) })
	t.Run("cancel_post_commit_fence", func(t *testing.T) { runCancelPostCommitFence(t, fe) })
	t.Run("commit_before_response_loss", func(t *testing.T) { runCommitBeforeResponseLoss(t, fe) })
	t.Run("stale_generation_complete", func(t *testing.T) { runStaleGeneration(t, fe) })
	t.Run("worker_unreachable_bench", func(t *testing.T) { runWorkerUnreachable(t, fe) })
	t.Run("quorum_loss_uncertain_write", func(t *testing.T) { runQuorumLoss(t, fe) })
	t.Run("durable_event_before_delivery_crash", func(t *testing.T) { runDurableEventCrash(t, fe) })

	doneCtx, doneCancel := context.WithTimeout(ctx, 30*time.Second)
	defer doneCancel()
	if _, err := cluster.RequestHost(doneCtx, kube, env.Namespace, cluster.HostRequest{
		RequestID: uuid.NewString(),
		Action:    cluster.ActionDone,
	}); err != nil {
		t.Logf("host done: %v", err)
	}
	if err := cluster.WriteRecords(ctx, kube, env.Namespace, "core_events", sink.Events()); err != nil {
		t.Fatalf("inconclusive: persist recorder events: %v", err)
	}
}
