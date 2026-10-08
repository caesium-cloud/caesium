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
	"github.com/caesium-cloud/caesium/test/robustness/recorder"
	"github.com/google/uuid"
	"k8s.io/client-go/kubernetes"
)

// These scenarios use the owned three-node fixture and real public admission.
// They run under TestOwnerCrash, whose host controller requires named passes.
func runCronSingleAdmission(t *testing.T, kube *kubernetes.Clientset, api *cluster.HTTP, env cluster.Env) {
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Second)
	defer cancel()
	topo := mustReadyTopology(t, ctx, kube, env)
	base := topo.Members[0].HTTPBase()
	alias := "cron-single-" + uuid.NewString()
	def := cluster.ProbeDefinition(alias, "", env.TaskImage)
	def.Trigger = jobdef.Trigger{Type: jobdef.TriggerCron, Configuration: map[string]any{"cron": "* * * * *"}}
	def.Steps[0].Command = []string{"sh", "-c", "echo CRON_SINGLE_ADMISSION"}
	if err := api.Apply(ctx, base, []jobdef.Definition{def}); err != nil {
		t.Fatal(err)
	}
	job, err := api.JobByAlias(ctx, base, alias)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, finish := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer finish()
		status, _, err := api.Do(cleanup, http.MethodPut, base+"/v1/jobs/"+job.ID+"/pause", nil)
		if err != nil || status != http.StatusOK {
			t.Errorf("pause cron fixture: status=%d error=%v", status, err)
		}
	})
	var admissions []struct {
		ID     string            `json:"id"`
		Status string            `json:"status"`
		Params map[string]string `json:"params"`
	}
	if err := cluster.Poll(ctx, 500*time.Millisecond, func() (bool, error) {
		status, raw, err := api.Do(ctx, http.MethodGet, base+"/v1/jobs/"+job.ID+"/runs", nil)
		if err != nil || status != http.StatusOK {
			return false, fmt.Errorf("cron run listing: status=%d error=%w", status, err)
		}
		if err := json.Unmarshal(raw, &admissions); err != nil {
			return false, err
		}
		seen := map[string]string{}
		completed := true
		for _, admission := range admissions {
			logical := admission.Params["logical_date"]
			date, err := time.Parse(time.RFC3339, logical)
			if err != nil || date.Second() != 0 || date.Nanosecond() != 0 {
				return false, fmt.Errorf("invalid cron logical_date %q", logical)
			}
			if previous, exists := seen[logical]; exists {
				return false, fmt.Errorf("duplicate cron tick %s: runs %s and %s", logical, previous, admission.ID)
			}
			seen[logical] = admission.ID
			if admission.Status == "failed" || admission.Status == "cancelled" {
				return false, fmt.Errorf("cron run %s ended %s", admission.ID, admission.Status)
			}
			completed = completed && admission.Status == "succeeded"
		}
		return len(seen) >= 2 && completed, nil
	}); err != nil {
		t.Fatal(err)
	}
	// All three trigger listeners must get time to fire this second boundary.
	// A single early listing could otherwise hide a follower's later duplicate.
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-timer.C:
	}
	status, raw, err := api.Do(ctx, http.MethodGet, base+"/v1/jobs/"+job.ID+"/runs", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("final cron listing: status=%d error=%v", status, err)
	}
	if err := json.Unmarshal(raw, &admissions); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, admission := range admissions {
		logical := admission.Params["logical_date"]
		if logical == "" || seen[logical] {
			t.Fatalf("duplicate/missing cron logical date in final listing: %s", raw)
		}
		seen[logical] = true
	}
	if err := cluster.WriteRecords(ctx, kube, env.Namespace, "cron_single_admission", map[string]any{"job_id": job.ID, "members": len(topo.Members), "admissions": admissions}); err != nil {
		t.Fatal(err)
	}
}

func runGracefulTriggerShutdown(t *testing.T, kube *kubernetes.Clientset, api *cluster.HTTP, sink *recorder.Sink, env cluster.Env) {
	ctx, cancel := context.WithTimeout(t.Context(), 240*time.Second)
	defer cancel()
	topo := mustReadyTopology(t, ctx, kube, env)
	membership, err := cluster.WaitMembership(ctx, dqliteAddresses(topo))
	if err != nil {
		t.Fatal(err)
	}
	leader, ok := topo.ByIP(cluster.HostIP(membership.Leader.Address))
	if !ok {
		t.Fatal("leader is not an owned Caesium member")
	}
	triggerNode := topo.Survivors(leader)[0]
	alias := "graceful-trigger-" + uuid.NewString()
	def := cluster.FixtureDefinition(alias, env.TaskImage)
	// Four independent manual admissions exceed the trigger node's two worker
	// slots. Pick one positively observed remote claim, finish all other runs,
	// then restart only the triggering server with no local task in flight.
	block := def.Steps[0]
	block.Next, block.DependsOn = nil, nil
	def.Steps = []jobdef.Step{block}
	def.Metadata.MaxParallelTasks = 1
	if err := api.Apply(ctx, triggerNode.HTTPBase(), []jobdef.Definition{def}); err != nil {
		t.Fatal(err)
	}
	job, err := api.JobByAlias(ctx, triggerNode.HTTPBase(), alias)
	if err != nil {
		t.Fatal(err)
	}
	var candidates []cluster.Run
	for range 4 {
		admitted, _, err := api.TriggerRun(ctx, triggerNode.HTTPBase(), job.ID)
		if err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, admitted)
		t.Cleanup(func() { sink.Release(admitted.ID) })
	}
	observer := leader.HTTPBase()
	var admitted cluster.Run
	var remote cluster.TaskRecipe
	if err := cluster.Poll(ctx, 200*time.Millisecond, func() (bool, error) {
		for _, candidate := range candidates {
			rows, err := api.QueryTaskRecipes(ctx, observer, candidate.ID)
			if err != nil {
				return false, err
			}
			for _, row := range rows {
				if row.Status == "running" && row.ClaimedBy != "" && row.ClaimedBy != triggerNode.NodeAddress && len(sink.StartsFor(candidate.ID, cluster.BlockStep)) > 0 {
					if _, owned := topo.ByNodeAddress(row.ClaimedBy); !owned {
						return false, fmt.Errorf("remote claim belongs to unknown node %s", row.ClaimedBy)
					}
					admitted, remote = candidate, row
					return true, nil
				}
			}
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		if candidate.ID != admitted.ID {
			sink.Release(candidate.ID)
		}
	}
	if err := cluster.Poll(ctx, 200*time.Millisecond, func() (bool, error) {
		for _, candidate := range candidates {
			if candidate.ID == admitted.ID {
				continue
			}
			got, err := api.GetRun(ctx, observer, job.ID, candidate.ID)
			if err != nil {
				return false, err
			}
			if got.Status != "succeeded" {
				return false, nil
			}
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := cluster.RefreshMember(ctx, kube, env.Namespace, triggerNode.Name)
	if err != nil {
		t.Fatal(err)
	}
	var oldRestarts int32
	var oldStarted time.Time
	for _, status := range before.Pod.Status.ContainerStatuses {
		if status.Name == cluster.CaesiumContainer && status.State.Running != nil {
			oldRestarts = status.RestartCount
			oldStarted = status.State.Running.StartedAt.Time
		}
	}
	if before.ContainerID == "" || oldStarted.IsZero() {
		t.Fatal("original server lacks a running container identity")
	}
	ack, err := cluster.RequestHost(ctx, kube, env.Namespace, cluster.HostRequest{
		RequestID: uuid.NewString(), Action: cluster.ActionTerminate,
		OwnerPod: before.Name, OwnerKindNode: before.Node,
		OwnerContainerID: before.ContainerID, RunID: admitted.ID,
	})
	if err != nil || ack.Evidence == "" {
		t.Fatalf("deliver SIGTERM to exact owned server: %+v error=%v", ack, err)
	}
	var after cluster.Member
	if err := cluster.Poll(ctx, 200*time.Millisecond, func() (bool, error) {
		member, err := cluster.RefreshMember(ctx, kube, env.Namespace, before.Name)
		if err != nil {
			return false, err
		}
		if member.UID != before.UID || member.IP != before.IP {
			return false, fmt.Errorf("server pod/sandbox identity changed during process restart")
		}
		for _, status := range member.Pod.Status.ContainerStatuses {
			if status.Name != cluster.CaesiumContainer || status.State.Running == nil || status.LastTerminationState.Terminated == nil ||
				member.ContainerID == "" || member.ContainerID == before.ContainerID || status.RestartCount <= oldRestarts ||
				!status.State.Running.StartedAt.After(oldStarted) {
				continue
			}
			terminated := status.LastTerminationState.Terminated
			if strings.TrimPrefix(terminated.ContainerID, "containerd://") != before.ContainerID {
				return false, fmt.Errorf("termination evidence identifies another container: %s", terminated.ContainerID)
			}
			if terminated.ExitCode != 0 {
				return false, fmt.Errorf("server shutdown exited %d", terminated.ExitCode)
			}
			after = member
			return true, nil
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	retained, err := api.GetRun(ctx, observer, job.ID, admitted.ID)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := api.QueryTaskRecipes(ctx, observer, admitted.ID)
	if err != nil {
		t.Fatal(err)
	}
	var retainedRemote cluster.TaskRecipe
	for _, row := range rows {
		if row.ID == remote.ID {
			retainedRemote = row
		}
	}
	if err := cluster.WriteRecords(ctx, kube, env.Namespace, "graceful_trigger_shutdown_retained", map[string]any{
		"run_id": admitted.ID, "trigger_pod": before.Name, "pod_uid": before.UID,
		"old_container": before.ContainerID, "new_container": after.ContainerID,
		"sigterm": ack, "remote_before": remote, "retained": retained, "remote_after": rows,
	}); err != nil {
		t.Fatal(err)
	}
	if retained.Status != "running" {
		t.Fatalf("run must remain running after trigger-node SIGTERM: %+v", retained)
	}
	if retainedRemote.Status != "running" || retainedRemote.ClaimedBy != remote.ClaimedBy || retainedRemote.ClaimAttempt != remote.ClaimAttempt {
		t.Fatalf("healthy remote claim changed across trigger-node shutdown: before=%+v after=%+v", remote, retainedRemote)
	}
	sink.Release(admitted.ID)
	var final cluster.Run
	if err := cluster.Poll(ctx, 500*time.Millisecond, func() (bool, error) {
		got, err := api.GetRun(ctx, observer, job.ID, admitted.ID)
		if err != nil {
			return false, err
		}
		if got.Status == "failed" || got.Status == "cancelled" {
			return false, fmt.Errorf("graceful shutdown terminal-failed run: %+v", got)
		}
		final = got
		return got.Status == "succeeded", nil
	}); err != nil {
		t.Fatal(err)
	}
	finalRecipes, err := api.QueryTaskRecipes(ctx, observer, admitted.ID)
	if err != nil {
		t.Fatal(err)
	}
	starts, completions := sink.StartsFor(admitted.ID, cluster.BlockStep), sink.CompletionsFor(admitted.ID, cluster.BlockStep)
	// Persist the observed rows/effects before judging them, including any
	// recovery fence that advances after the healthy remote claim checkpoint.
	if err := cluster.WriteRecords(ctx, kube, env.Namespace, "graceful_trigger_shutdown", map[string]any{
		"run_id": admitted.ID, "trigger_pod": before.Name, "pod_uid": before.UID,
		"old_container": before.ContainerID, "new_container": after.ContainerID,
		"sigterm": ack, "remote_before": remote, "remote_after": retainedRemote,
		"remote_final": finalRecipes, "starts": starts, "completions": completions, "final": final,
	}); err != nil {
		t.Fatal(err)
	}
	if err := validateGracefulShutdownResult(admitted.ID, job.ID, remote, final, finalRecipes, starts, completions); err != nil {
		t.Fatalf("shutdown recovery invariants: %v; before=%+v final=%+v starts=%+v completions=%+v", err, remote, finalRecipes, starts, completions)
	}
	if err := cluster.Poll(ctx, time.Second, func() (bool, error) {
		return api.Health(ctx, triggerNode.HTTPBase()) == nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Owner recovery has at-least-once task execution: it resets in-flight rows and
// advances the claim attempt on re-dispatch. A same-owner restart can retain its
// generation. Durable identities/retry attempts must survive, and each extra
// execution requires an observed extra claim without a generation regression.
func validateGracefulShutdownResult(runID, jobID string, before cluster.TaskRecipe, final cluster.Run, recipes []cluster.TaskRecipe, starts, completions []recorder.Event) error {
	if final.ID != runID || final.JobID != jobID || final.Status != "succeeded" || len(final.Tasks) != 1 || len(recipes) != 1 {
		return fmt.Errorf("run identity, task set or final success changed")
	}
	task, recipe := final.Tasks[0], recipes[0]
	// Compact run tasks expose the catalog ID in both id and task_id; the
	// partition recipe below carries the distinct durable TaskRun ID.
	if task.ID != before.TaskID || task.TaskID != before.TaskID || task.Attempt != before.Attempt || task.Status != "succeeded" ||
		recipe.ID != before.ID || recipe.TaskID != before.TaskID || recipe.Attempt != before.Attempt || recipe.Status != "succeeded" ||
		recipe.Image != before.Image || recipe.Command != before.Command {
		return fmt.Errorf("durable task identity, retry attempt, recipe or final success changed")
	}
	if before.ClaimAttempt <= 0 || before.OwnerGeneration <= 0 || recipe.ClaimAttempt < before.ClaimAttempt || recipe.OwnerGeneration < before.OwnerGeneration {
		return fmt.Errorf("claim attempt or owner generation missing/regressed")
	}
	if len(starts) == 0 || len(starts) != len(completions) {
		return fmt.Errorf("missing or unpaired execution evidence")
	}
	if len(starts) > 1 && (recipe.ClaimAttempt == before.ClaimAttempt ||
		len(starts)-1 > recipe.ClaimAttempt-before.ClaimAttempt) {
		return fmt.Errorf("multiple executions lack advanced recovery fence")
	}
	byNonce := make(map[string]time.Time, len(starts))
	for _, event := range starts {
		if event.Kind != "start" || event.RunID != runID || event.Step != cluster.BlockStep || strings.TrimSpace(event.Nonce) == "" || event.At.IsZero() {
			return fmt.Errorf("unbound start evidence")
		}
		if _, exists := byNonce[event.Nonce]; exists {
			return fmt.Errorf("duplicate start nonce")
		}
		byNonce[event.Nonce] = event.At
	}
	for _, event := range completions {
		started, exists := byNonce[event.Nonce]
		if event.Kind != "complete" || event.RunID != runID || event.Step != cluster.BlockStep || !exists || event.At.Before(started) {
			return fmt.Errorf("unbound, repeated or premature completion evidence")
		}
		delete(byNonce, event.Nonce)
	}
	return nil
}
