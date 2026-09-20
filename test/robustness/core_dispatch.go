//go:build integration

package robustness

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/test/robustness/cluster"
	"github.com/caesium-cloud/caesium/test/robustness/faults"
)

func runWorkerUnreachable(t *testing.T, fe *faultEnv) {
	refreshTopo(t, fe)
	ctx := context.Background()
	owner := fe.leader
	worker := otherMember(fe.topo, owner)
	live, err := cluster.RefreshMember(ctx, fe.kube, fe.env.Namespace, worker.Name)
	if err != nil || live.ContainerID == "" {
		t.Fatalf("refresh worker %s: %v", worker.Name, err)
	}

	beforeRejected := scrapeCounter(t, fe, owner, MetricDispatchRejected, map[string]string{"reason": DispatchReasonNetworkError})
	beforeStalled := scrapeCounter(t, fe, owner, MetricDispatchStalled, map[string]string{"reason": "no_capacity"})
	beforeSent := scrapeCounter(t, fe, owner, MetricDispatchSent, nil)

	stateBefore, rawBefore, err := fe.host.TaskState(ctx, live.Node, live.ContainerID)
	if err != nil {
		t.Fatalf("worker task state: %v (%s)", err, truncate([]byte(rawBefore), 200))
	}
	ev := faults.PauseEvidence{
		ContainerID: live.ContainerID, StateBefore: stateBefore,
		LeaseTTLSeconds: 0, ObservedNodeAddr: live.NodeAddress,
		ReachableBefore: fe.httpAPI.Health(ctx, live.HTTPBase()) == nil,
	}
	resumed := false
	t.Cleanup(func() {
		if resumed {
			return
		}
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, _, _ = fe.host.Resume(cctx, live.Node, live.ContainerID)
	})

	pauseCtx, pauseCancel := context.WithTimeout(ctx, 2*time.Minute)
	pausedState, pausedRaw, err := fe.host.Pause(pauseCtx, live.Node, live.ContainerID)
	pauseCancel()
	if err != nil {
		t.Fatalf("pause worker: %v (%s)", err, truncate([]byte(pausedRaw), 400))
	}
	ev.StatePaused = pausedState
	probe := &cluster.HTTP{Client: &http.Client{Timeout: 3 * time.Second}, ManualKey: fe.env.ManualKey}
	unreachCtx, unreachCancel := context.WithTimeout(ctx, 60*time.Second)
	var lastHealth error
	if err := cluster.Poll(unreachCtx, time.Second, func() (bool, error) {
		lastHealth = probe.Health(unreachCtx, live.HTTPBase())
		return lastHealth != nil, nil
	}); err != nil {
		unreachCancel()
		t.Fatalf("paused worker still answered /health")
	}
	unreachCancel()
	ev.DuringPauseErr = lastHealth.Error()
	if err := ev.Activated(); err != nil {
		t.Fatalf("worker pause activation: %v", err)
	}
	requireFaultActivated(t, true, "worker unreachable")

	job, alias := applyFaultFixture(t, fe, owner, "bench", 4)
	var runIDs []string
	var progressClaims []string
	for i := 0; i < 4; i++ {
		run, _, err := fe.httpAPI.TriggerRun(ctx, owner.HTTPBase(), job.ID)
		if err != nil {
			t.Fatalf("trigger %s #%d: %v", alias, i+1, err)
		}
		runIDs = append(runIDs, run.ID)
	}

	metricCtx, metricCancel := context.WithTimeout(ctx, 2*time.Minute)
	var afterRejected float64
	if err := cluster.Poll(metricCtx, 2*time.Second, func() (bool, error) {
		afterRejected = scrapeCounter(t, fe, owner, MetricDispatchRejected, map[string]string{"reason": DispatchReasonNetworkError})
		return afterRejected > beforeRejected, nil
	}); err != nil {
		metricCancel()
		t.Fatalf("caesium_dispatch_rejected_total{reason=network_error} did not rise on %s (before=%.0f after=%.0f); dispatch progress against the unreachable worker is unproven",
			owner.Name, beforeRejected, afterRejected)
	}
	metricCancel()
	afterSent := scrapeCounter(t, fe, owner, MetricDispatchSent, nil)
	afterStalled := scrapeCounter(t, fe, owner, MetricDispatchStalled, map[string]string{"reason": "no_capacity"})

	claimCtx, claimCancel := context.WithTimeout(ctx, 2*time.Minute)
	_ = cluster.Poll(claimCtx, time.Second, func() (bool, error) {
		progressClaims = nil
		for _, id := range runIDs {
			got, gerr := fe.httpAPI.GetRun(claimCtx, owner.HTTPBase(), job.ID, id)
			if gerr != nil {
				return false, nil
			}
			for _, tr := range got.Tasks {
				if strings.TrimSpace(tr.ClaimedBy) == "" {
					continue
				}
				if cluster.HostIP(tr.ClaimedBy) == live.IP {
					continue
				}
				progressClaims = append(progressClaims, tr.ClaimedBy)
			}
		}
		return len(progressClaims) > 0, nil
	})
	claimCancel()
	if len(progressClaims) == 0 {
		t.Fatalf("no task was claimed by a reachable peer while %s was unreachable; benching is unproven (rejected Δ=%.0f sent %.0f→%.0f)",
			worker.Name, afterRejected-beforeRejected, beforeSent, afterSent)
	}

	resumeCtx, resumeCancel := context.WithTimeout(ctx, 2*time.Minute)
	resumedState, resumedRaw, err := fe.host.Resume(resumeCtx, live.Node, live.ContainerID)
	resumeCancel()
	resumed = true
	if err != nil {
		t.Fatalf("resume worker: %v (%s)", err, truncate([]byte(resumedRaw), 400))
	}
	ev.StateResumed = resumedState
	healCtx, healCancel := context.WithTimeout(ctx, 3*time.Minute)
	err = cluster.Poll(healCtx, time.Second, func() (bool, error) {
		return fe.httpAPI.Health(healCtx, live.HTTPBase()) == nil, nil
	})
	healCancel()
	ev.ReachableAfter = err == nil
	if err := ev.Healed(); err != nil {
		t.Fatalf("worker resume: %v", err)
	}
	waitMembership(t, fe, 3*time.Minute)

	// peerBenchCooldown is 10s. Wait past it so the recovered worker is
	// eligible again, then prove it receives new work.
	time.Sleep(12 * time.Second)
	var recoveredClaim cluster.Task
	recovered := false
	for i := 0; i < 6 && !recovered; i++ {
		run, _, err := fe.httpAPI.TriggerRun(ctx, owner.HTTPBase(), job.ID)
		if err != nil {
			t.Fatalf("post-cooldown trigger: %v", err)
		}
		runIDs = append(runIDs, run.ID)
		waitCtx, waitCancel := context.WithTimeout(ctx, 45*time.Second)
		got, ok := waitClaimedBy(t, waitCtx, fe, owner.HTTPBase(), job.ID, run.ID, live.IP)
		waitCancel()
		if ok {
			recoveredClaim = got
			recovered = true
		}
	}
	if !recovered {
		t.Fatalf("worker %s received no new claim after cooldown; bench expiry is unproven", worker.Name)
	}

	writeCoreRecord(t, fe, "worker_unreachable_bench", map[string]any{
		"worker":              worker.Name,
		"owner":               owner.Name,
		"rejected_before":     beforeRejected,
		"rejected_after":      afterRejected,
		"stalled_before":      beforeStalled,
		"stalled_after":       afterStalled,
		"sent_before":         beforeSent,
		"sent_after":          afterSent,
		"progress_claims":     progressClaims,
		"recovered_claim":     recoveredClaim,
		"run_ids":             runIDs,
		"pause_state_paused":  pausedState,
		"pause_state_resumed": resumedState,
		"cooldown_seconds":    12,
	})
}
