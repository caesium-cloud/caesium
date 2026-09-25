//go:build integration

package robustness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

const coreLeaseTTLSeconds = 30.0

func writeCoreRecord(t *testing.T, fe *faultEnv, key string, record map[string]any) {
	t.Helper()
	record["redacted"] = true
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("inconclusive: marshal %s: %v", key, err)
	}
	if err := cluster.WriteRecords(context.Background(), fe.kube, fe.env.Namespace, key, json.RawMessage(RedactSecrets(string(raw)))); err != nil {
		t.Fatalf("inconclusive: persist %s records: %v", key, err)
	}
}

func applyCoreFixture(t *testing.T, fe *faultEnv, member cluster.Member, name, alias string) cluster.Job {
	t.Helper()
	ctx := context.Background()
	def, err := LoadCoreFixture(name, alias, fe.env.TaskImage)
	if err != nil {
		t.Fatalf("load fixture %s: %v", name, err)
	}
	if err := def.Validate(); err != nil {
		t.Fatalf("fixture %s invalid: %v", alias, err)
	}
	if err := fe.httpAPI.Apply(ctx, member.HTTPBase(), []jobdef.Definition{def}); err != nil {
		t.Fatalf("apply %s: %v", alias, err)
	}
	job, err := fe.httpAPI.JobByAlias(ctx, member.HTTPBase(), alias)
	if err != nil {
		t.Fatalf("read applied job %s: %v", alias, err)
	}
	return job
}

func uniqueAlias(kind string) string {
	return fmt.Sprintf("core-%s-%s", kind, strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
}

func fingerprintRun(t *testing.T, ctx context.Context, fe *faultEnv, base, jobID, runID string) StateFingerprint {
	t.Helper()
	got, err := fe.httpAPI.GetRun(ctx, base, jobID, runID)
	if err != nil {
		t.Fatalf("snapshot get run %s: %v", runID, err)
	}
	fp := StateFingerprint{RunID: got.ID, JobID: got.JobID, Status: got.Status}
	if lease, lerr := fe.httpAPI.QueryLease(ctx, base, runID); lerr == nil {
		fp.LeaseOwner = lease.OwnerNode
		fp.LeaseGeneration = lease.Generation
	}
	recipes, rerr := fe.httpAPI.QueryTaskRecipes(ctx, base, runID)
	recipeByID := map[string]cluster.TaskRecipe{}
	if rerr == nil {
		for _, r := range recipes {
			recipeByID[r.ID] = r
		}
	}
	for _, tr := range got.Tasks {
		tf := TaskFingerprint{
			ID: tr.ID, TaskID: tr.TaskID, Status: tr.Status,
			Image: tr.Image, ClaimedBy: tr.ClaimedBy, Attempt: tr.Attempt, Error: tr.Error,
		}
		if r, ok := recipeByID[tr.ID]; ok {
			tf.Image = r.Image
			tf.Command = r.Command
			if tf.ClaimedBy == "" {
				tf.ClaimedBy = r.ClaimedBy
			}
		}
		fp.Tasks = append(fp.Tasks, tf)
	}
	for _, ev := range fe.sink.Events() {
		if ev.RunID == runID && strings.TrimSpace(ev.Nonce) != "" {
			fp.EffectNonces = append(fp.EffectNonces, ev.Nonce)
		}
	}
	return fp
}

func requireNoMutation(t *testing.T, before, after StateFingerprint, what string) {
	t.Helper()
	if diffs := StateDiffs(before, after); len(diffs) > 0 {
		t.Fatalf("%s mutated observable state: %v", what, diffs)
	}
}

func timedEffects(sink *recorder.Sink, runID string) []TimedStep {
	var out []TimedStep
	for _, ev := range sink.Events() {
		if ev.RunID != runID {
			continue
		}
		if ev.Kind != "start" && ev.Kind != "complete" {
			continue
		}
		out = append(out, TimedStep{Step: ev.Step, Kind: ev.Kind, At: ev.At})
	}
	return out
}

func internalToken(t *testing.T, fe *faultEnv) string {
	t.Helper()
	for _, m := range fe.topo.Members {
		if v := m.EnvValue("CAESIUM_INTERNAL_WAKEUP_TOKEN"); strings.TrimSpace(v) != "" {
			return v
		}
	}
	t.Fatalf("inconclusive: no member advertised CAESIUM_INTERNAL_WAKEUP_TOKEN")
	return ""
}

func validInternalClient(t *testing.T, fe *faultEnv, member cluster.Member) *cluster.InternalClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	token := internalToken(t, fe)
	cli, err := cluster.MintInternalClient(ctx, fe.httpAPI, member.HTTPBase(), token, token)
	if err != nil {
		t.Fatalf("mint internal client: %v", err)
	}
	return cli
}

func wrongTokenClient(t *testing.T, fe *faultEnv, member cluster.Member) *cluster.InternalClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	token := internalToken(t, fe)
	cli, err := cluster.MintInternalClient(ctx, fe.httpAPI, member.HTTPBase(), token, "wrong-internal-token-not-the-cluster-value")
	if err != nil {
		t.Fatalf("mint wrong-token client: %v", err)
	}
	return cli
}

func completePayload(run cluster.Run, lease cluster.Lease, status string, generation int64) map[string]any {
	payload := map[string]any{
		"run_id":           run.ID,
		"owner_generation": generation,
		"attempt":          1,
		"worker_node":      lease.OwnerNode,
		"status":           status,
	}
	if len(run.Tasks) > 0 {
		tr := run.Tasks[0]
		payload["task_id"] = tr.TaskID
		payload["task_run_id"] = tr.ID
		payload["attempt"] = tr.Attempt
		if strings.TrimSpace(tr.ClaimedBy) != "" {
			payload["worker_node"] = tr.ClaimedBy
		}
	}
	return payload
}

func dispatchPayload(run cluster.Run, lease cluster.Lease, worker string) map[string]any {
	payload := map[string]any{
		"run_id":           run.ID,
		"owner_generation": lease.Generation,
		"attempt":          1,
		"worker_node":      worker,
		"owner_base_url":   cluster.InternalBase(cluster.HostIP(lease.OwnerNode)),
		"deadline":         time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339Nano),
	}
	if len(run.Tasks) > 0 {
		tr := run.Tasks[0]
		payload["task_id"] = tr.TaskID
		payload["task_run_id"] = tr.ID
		payload["attempt"] = tr.Attempt
	}
	return payload
}

func waitMembership(t *testing.T, fe *faultEnv, timeout time.Duration) cluster.Membership {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	m, err := cluster.WaitMembership(ctx, dqliteAddresses(fe.topo))
	if err != nil {
		t.Fatalf("dqlite membership: %v", err)
	}
	return m
}

func refreshTopo(t *testing.T, fe *faultEnv) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fe.topo = mustReadyTopology(t, ctx, fe.kube, fe.env)
	memberCtx, memberCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer memberCancel()
	membership, err := cluster.WaitMembership(memberCtx, dqliteAddresses(fe.topo))
	if err != nil {
		t.Fatalf("membership after refresh: %v", err)
	}
	leader, ok := fe.topo.ByIP(cluster.HostIP(membership.Leader.Address))
	if !ok {
		t.Fatalf("leader %s is not a caesium pod", membership.Leader.Address)
	}
	fe.leader = leader
}

type isolation struct {
	plans    []faults.PartitionPlan
	minority cluster.Member
	majority []cluster.Member
	healed   bool
}

func isolateMember(t *testing.T, fe *faultEnv, minority cluster.Member) *isolation {
	t.Helper()
	ctx := context.Background()
	iso := &isolation{minority: minority}
	for _, m := range fe.topo.Members {
		if m.Name != minority.Name {
			iso.majority = append(iso.majority, m)
		}
	}
	if len(iso.majority) != 2 {
		t.Fatalf("need a 2-1 split, majority=%d", len(iso.majority))
	}
	minLive, err := cluster.RefreshMember(ctx, fe.kube, fe.env.Namespace, minority.Name)
	if err != nil {
		t.Fatalf("refresh minority %s: %v", minority.Name, err)
	}
	tagBase := "rb3" + strings.ReplaceAll(uuid.NewString(), "-", "")[:6]
	for i, other := range iso.majority {
		live, err := cluster.RefreshMember(ctx, fe.kube, fe.env.Namespace, other.Name)
		if err != nil {
			t.Fatalf("refresh %s: %v", other.Name, err)
		}
		fwd := faults.PartitionPlan{
			Tag:    fmt.Sprintf("%s%d", tagBase, i),
			SrcPod: minLive.Name, DstPod: live.Name,
			SrcIP: minLive.IP, DstIP: live.IP,
			SrcNode: minLive.Node, DstNode: live.Node,
			DropPorts:  []int{faults.PortAPI, faults.PortInternal, faults.PortRaft},
			CountPorts: []int{faults.PortRaft, faults.PortInternal},
		}
		rev := faults.PartitionPlan{
			Tag:    fmt.Sprintf("%s%dr", tagBase, i),
			SrcPod: live.Name, DstPod: minLive.Name,
			SrcIP: live.IP, DstIP: minLive.IP,
			SrcNode: live.Node, DstNode: minLive.Node,
			DropPorts:  []int{faults.PortAPI, faults.PortInternal, faults.PortRaft},
			CountPorts: []int{faults.PortRaft, faults.PortInternal},
		}
		iso.plans = append(iso.plans, fwd, rev)
	}
	t.Cleanup(func() { iso.heal(t, fe) })
	for i, p := range iso.plans {
		partCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		evidence, err := fe.host.Partition(partCtx, p)
		cancel()
		if err != nil {
			t.Fatalf("install partition %s: %v (%s)", p.Tag, err, truncate([]byte(evidence), 400))
		}
		iso.plans[i] = p
	}
	return iso
}

// requireSplitActive fails unless every plan's blocked raft or internal rule
// has matched packets and the minority no longer reports a dqlite leader.
func requireSplitActive(t *testing.T, fe *faultEnv, iso *isolation) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for _, p := range iso.plans {
		plan := p
		var blocked map[string]faults.Counter
		var raw string
		if err := cluster.Poll(ctx, 2*time.Second, func() (bool, error) {
			b, _, r, err := fe.host.Counters(ctx, plan)
			if err != nil {
				return false, nil
			}
			blocked, raw = b, r
			return SplitDropActive(b[plan.DropRuleComment(faults.PortRaft)].Packets, b[plan.DropRuleComment(faults.PortInternal)].Packets), nil
		}); err != nil {
			t.Fatalf("2-1 split unproven: plan %s never dropped raft/internal traffic (counters=%v raw=%s)",
				plan.Tag, blocked, truncate([]byte(raw), 800))
		}
		t.Logf("split plan %s drop raft=%d internal=%d", plan.Tag,
			blocked[plan.DropRuleComment(faults.PortRaft)].Packets,
			blocked[plan.DropRuleComment(faults.PortInternal)].Packets)
	}

	leader := iso.majority[0]
	if fe.leader.Name != iso.minority.Name {
		leader = fe.leader
	}
	target := fmt.Sprintf("http://%s:%d/health", leader.IP, faults.PortAPI)
	minLive, err := cluster.RefreshMember(ctx, fe.kube, fe.env.Namespace, iso.minority.Name)
	if err != nil {
		t.Fatalf("refresh minority: %v", err)
	}
	probeCtx, probeCancel := context.WithTimeout(ctx, 60*time.Second)
	fwd, err := fe.host.Probe(probeCtx, minLive.Node, minLive.ContainerID, iso.minority.Name, target, 4)
	probeCancel()
	if err != nil {
		t.Fatalf("inconclusive: minority probe failed to run: %v", err)
	}
	if fwd.Success {
		t.Fatalf("minority %s still reached majority %s; 2-1 split is not active", iso.minority.Name, leader.Name)
	}

	qctx, qcancel := context.WithTimeout(ctx, 30*time.Second)
	defer qcancel()
	if err := cluster.Poll(qctx, 2*time.Second, func() (bool, error) {
		call, cancel := context.WithTimeout(qctx, 4*time.Second)
		defer cancel()
		leaderInfo, _, qerr := cluster.QueryNode(call, iso.minority.DqliteAddr())
		if qerr != nil {
			return true, nil
		}
		if leaderInfo == nil || strings.TrimSpace(leaderInfo.Address) == "" {
			return true, nil
		}
		return false, nil
	}); err != nil {
		t.Fatalf("minority %s still reports a dqlite leader; 2-1 split is unproven", iso.minority.Name)
	}
}

func (iso *isolation) heal(t *testing.T, fe *faultEnv) {
	t.Helper()
	if iso == nil || iso.healed {
		return
	}
	iso.healed = true
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, p := range iso.plans {
		if _, err := fe.host.Heal(ctx, p); err != nil {
			t.Logf("heal %s: %v", p.Tag, err)
		}
	}
}

func scrapeCounter(t *testing.T, fe *faultEnv, member cluster.Member, name string, labels map[string]string) float64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	text, err := fe.httpAPI.Metrics(ctx, member.HTTPBase())
	if err != nil {
		t.Fatalf("scrape /metrics on %s: %v", member.Name, err)
	}
	v, _ := PromCounter(text, name, labels)
	return v
}

func instrumented() bool {
	return os.Getenv("CAESIUM_ROBUSTNESS_INSTRUMENTED") == "1"
}

func waitClaimedBy(t *testing.T, ctx context.Context, fe *faultEnv, base, jobID, runID string, wantIP string) (cluster.Task, bool) {
	t.Helper()
	var found cluster.Task
	ok := false
	_ = cluster.Poll(ctx, time.Second, func() (bool, error) {
		got, err := fe.httpAPI.GetRun(ctx, base, jobID, runID)
		if err != nil {
			return false, nil
		}
		for _, tr := range got.Tasks {
			if strings.TrimSpace(tr.ClaimedBy) == "" {
				continue
			}
			if wantIP == "" || cluster.HostIP(tr.ClaimedBy) == wantIP {
				found = tr
				ok = true
				return true, nil
			}
		}
		return false, nil
	})
	return found, ok
}

func requireHTTPStatus(t *testing.T, got int, want int, body string) {
	t.Helper()
	if got != want {
		t.Fatalf("status %d, want %d body=%s", got, want, truncate([]byte(body), 400))
	}
}

func digestOf(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256([]byte(RedactSecrets(string(raw))))
	return hex.EncodeToString(sum[:])
}

func waitRunStatus(t *testing.T, ctx context.Context, h *cluster.HTTP, base, jobID, runID, want string) cluster.Run {
	t.Helper()
	var final cluster.Run
	if err := cluster.Poll(ctx, time.Second, func() (bool, error) {
		got, err := h.GetRun(ctx, base, jobID, runID)
		if err != nil {
			return false, nil
		}
		final = got
		return strings.EqualFold(got.Status, want), nil
	}); err != nil {
		t.Fatalf("run %s did not become %s (last %s): %v", runID, want, final.Status, err)
	}
	return final
}

func requireFaultActivated(t *testing.T, ok bool, what string) {
	t.Helper()
	if !ok {
		t.Fatalf("missing fault activation: %s (cannot pass)", what)
	}
}

func waitRunHasTask(t *testing.T, ctx context.Context, fe *faultEnv, base, jobID, runID string) cluster.Run {
	t.Helper()
	wait, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var got cluster.Run
	if err := cluster.Poll(wait, time.Second, func() (bool, error) {
		run, err := fe.httpAPI.GetRun(wait, base, jobID, runID)
		if err != nil {
			return false, nil
		}
		got = run
		return len(run.Tasks) > 0, nil
	}); err != nil {
		t.Fatalf("run %s never exposed a task: %v", runID, err)
	}
	return got
}

func memberByNode(t *testing.T, fe *faultEnv, addr string) cluster.Member {
	t.Helper()
	if m, ok := fe.topo.ByNodeAddress(addr); ok {
		return m
	}
	if m, ok := fe.topo.ByIP(cluster.HostIP(addr)); ok {
		return m
	}
	t.Fatalf("no member for node address %s", addr)
	return cluster.Member{}
}

func applyBlockedRun(t *testing.T, fe *faultEnv, member cluster.Member, kind string) (cluster.Job, cluster.Run, cluster.Lease) {
	t.Helper()
	ctx := context.Background()
	alias := uniqueAlias(kind)
	def := cluster.FixtureDefinition(alias, fe.env.TaskImage)
	if err := fe.httpAPI.Apply(ctx, member.HTTPBase(), []jobdef.Definition{def}); err != nil {
		t.Fatalf("apply blocked fixture: %v", err)
	}
	job, err := fe.httpAPI.JobByAlias(ctx, member.HTTPBase(), alias)
	if err != nil {
		t.Fatalf("read blocked job: %v", err)
	}
	run, _, err := fe.httpAPI.TriggerRun(ctx, member.HTTPBase(), job.ID)
	if err != nil {
		t.Fatalf("trigger blocked fixture: %v", err)
	}
	leaseCtx, leaseCancel := context.WithTimeout(ctx, 90*time.Second)
	lease, err := cluster.WaitLease(leaseCtx, fe.httpAPI, member.HTTPBase(), run.ID, "")
	leaseCancel()
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	startCtx, startCancel := context.WithTimeout(ctx, 90*time.Second)
	if err := cluster.Poll(startCtx, 500*time.Millisecond, func() (bool, error) {
		return len(fe.sink.StartsFor(run.ID, cluster.BlockStep)) > 0, nil
	}); err != nil {
		startCancel()
		t.Fatalf("block start missing: %v", err)
	}
	startCancel()
	runCtx, runCancel := context.WithTimeout(ctx, 90*time.Second)
	var detail cluster.Run
	if err := cluster.Poll(runCtx, time.Second, func() (bool, error) {
		got, gerr := fe.httpAPI.GetRun(runCtx, member.HTTPBase(), job.ID, run.ID)
		if gerr != nil {
			return false, nil
		}
		detail = got
		for _, tr := range got.Tasks {
			if strings.EqualFold(tr.Status, "running") {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		runCancel()
		t.Fatalf("block task never running: %v", err)
	}
	runCancel()
	t.Cleanup(func() { fe.sink.Release(run.ID) })
	return job, detail, lease
}

func taskStatusByName(run cluster.Run, names map[string]string, step string) string {
	for _, tr := range run.Tasks {
		if names[tr.TaskID] == step {
			return tr.Status
		}
	}
	return ""
}
