//go:build integration

package lifecycle

// H1 (#582 follow-up): after a disk-loss replacement settles, the lost member's
// raft entry is removed with the documented operator command and must be gone
// from every member's view.
//
// Each disk-loss case now runs in two runner phases around a host step:
//
//  1. TestLifecycleClusterJoiningOrdinalOne / TestLifecycleClusterOrdinalZeroLoss
//     prove the replacement joined and settled (three live voters, every stale
//     entry non-voting) and write cluster-<stage>-removal-plan.json instead of
//     a pass record.
//  2. The host runs `caesium system nodes remove <id> --json` inside a live
//     follower through kubectl exec, exactly as an operator would: first the
//     leader and a live voter (both must be refused), then every stale entry,
//     retrying a retryable refusal. It records every attempt's exit code,
//     stdout and stderr in cluster-<stage>-removal-host.json.
//  3. TestLifecycleCluster<Stage>Removed judges those attempts and polls every
//     member's direct Cluster RPC and /health cluster view until the stale
//     entries are gone, then writes the case record.
//
// A missing plan or host record is blocked; a refusal of a stale entry, a
// removal of a live member, or an entry that stays is a failed case.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/test/robustness/cluster"
)

func removalPlanFile(stage string) string { return "cluster-" + stage + "-removal-plan.json" }
func removalHostFile(stage string) string { return "cluster-" + stage + "-removal-host.json" }

// directViews reads every member's own Leader/Cluster answer.
func directViews(t *testing.T, topo cluster.Topology) ([]ordinalZeroView, error) {
	t.Helper()
	views := make([]ordinalZeroView, 0, len(topo.Members))
	for _, m := range topo.Members {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		leader, members, err := cluster.QueryNode(ctx, m.DqliteAddr())
		cancel()
		if err != nil || leader == nil {
			return nil, fmt.Errorf("%s: leader=%v err=%v", m.Name, leader, err)
		}
		views = append(views, newOrdinalZeroView(m.Name, m.DqliteAddr(), *leader, members))
	}
	return views, nil
}

// settleForStaleRemoval waits for a replaced member's cluster to settle (one
// leader and membership in every view, voters exactly the live members, every
// stale entry non-voting) and writes the removal plan. Role adjustment runs
// every 30 s, so a lost voter is demoted within about a minute of its
// replacement's promotion.
func settleForStaleRemoval(t *testing.T, caseName, stage string, topo cluster.Topology, freshAddr string,
	staleIDs []uint64, detail string, evidence map[string]any) {
	t.Helper()
	liveAddrs := make([]string, 0, len(topo.Members))
	for _, m := range topo.Members {
		liveAddrs = append(liveAddrs, m.DqliteAddr())
	}
	var views []ordinalZeroView
	var membership ordinalZeroMembership
	var settleErr, rpcErr error
	deadline := time.Now().Add(3 * time.Minute)
	for {
		var round []ordinalZeroView
		round, rpcErr = directViews(t, topo)
		if rpcErr == nil {
			views = round
			freshID := uint64(0)
			for _, n := range views[0].nodes {
				if n.Address == freshAddr {
					freshID = n.ID
				}
			}
			membership, settleErr = ordinalZeroSettled(views, freshID, freshAddr, liveAddrs, staleIDs)
			if settleErr == nil {
				break
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if views == nil {
		blockf(t, caseName, "direct dqlite RPC unanswered after 3m: %v", rpcErr)
	}
	evidence["settled_direct_cluster_views"] = views
	evidence["settled_voter_addresses"] = membership.VoterAddresses
	evidence["settled_stale_entries"] = membership.StaleEntries
	if settleErr != nil {
		writeCase(t, caseRecord{Name: caseName, Status: statusFail, Detail: settleErr.Error(), Observations: evidence})
		t.Fatalf("FAIL %s: %v", caseName, settleErr)
	}
	writeStaleRemovalPlan(t, caseName, stage, topo, views, staleIDs, detail, evidence)
}

// writeStaleRemovalPlan picks where the operator command runs and what it must
// refuse and remove. It runs in a follower, so the removal is forwarded to the
// leader, and the two controls are the leader and the third voter: removing
// either must be refused with exactly "leader" and "voter".
func writeStaleRemovalPlan(t *testing.T, caseName, stage string, topo cluster.Topology, views []ordinalZeroView,
	staleIDs []uint64, detail string, evidence map[string]any) {
	t.Helper()
	leaderAddr := ""
	if _, addr, ok := strings.Cut(views[0].Leader, "/"); ok {
		leaderAddr = addr
	}
	byAddr := map[string]string{}
	liveAddrs := make([]string, 0, len(topo.Members))
	for _, m := range topo.Members {
		byAddr[m.DqliteAddr()] = m.Name
		liveAddrs = append(liveAddrs, m.DqliteAddr())
	}
	sort.Strings(liveAddrs)
	lost := map[uint64]bool{}
	for _, id := range staleIDs {
		lost[id] = true
	}
	rawEvidence, err := json.Marshal(evidence)
	if err != nil {
		blockf(t, caseName, "marshal first-phase evidence: %v", err)
	}
	plan := staleRemovalPlan{LifecycleID: mustEnv(t, "CAESIUM_LIFECYCLE_ID"), Case: caseName, Stage: stage,
		LiveAddrs: liveAddrs, Detail: detail, Evidence: rawEvidence}
	var followers []staleRemovalTarget
	for _, n := range views[0].nodes {
		target := staleRemovalTarget{ID: strconv.FormatUint(n.ID, 10), Address: n.Address, Role: strings.ToLower(n.Role.String())}
		switch {
		case byAddr[n.Address] == "" || lost[n.ID]:
			plan.Stale = append(plan.Stale, target)
		case n.Address == leaderAddr:
			plan.Leader = target
		default:
			followers = append(followers, target)
		}
	}
	sort.Slice(followers, func(i, j int) bool { return byAddr[followers[i].Address] < byAddr[followers[j].Address] })
	if plan.Leader.ID == "" || len(followers) != 2 || len(plan.Stale) == 0 {
		blockf(t, caseName, "cannot plan the removal: leader %q, %d live followers, %d stale entries in %v",
			views[0].Leader, len(followers), len(plan.Stale), views[0].Members)
	}
	plan.Executor = byAddr[followers[0].Address]
	plan.Voter = followers[1]
	for _, id := range staleIDs {
		if id != 0 {
			plan.StaleIDs = append(plan.StaleIDs, strconv.FormatUint(id, 10))
		}
	}
	writeJSON(t, removalPlanFile(stage), plan)
	t.Logf("%s: settled; the host must refuse leader %s and voter %s and remove %v through %s",
		caseName, plan.Leader.ID, plan.Voter.ID, plan.Stale, plan.Executor)
}

// clusterHealthViews reads every member's /health cluster view. /health can
// answer 503 while the body is still the report, so the body is read whatever
// the status.
func clusterHealthViews(t *testing.T, h *cluster.HTTP, topo cluster.Topology) ([]clusterHealthView, error) {
	t.Helper()
	out := make([]clusterHealthView, 0, len(topo.Members))
	for _, m := range topo.Members {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		status, raw, err := h.Do(ctx, http.MethodGet, m.HTTPBase()+"/health", nil)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("%s /health: %w", m.Name, err)
		}
		var body struct {
			Checks struct {
				Cluster *struct {
					Status  string                `json:"status"`
					Members []clusterHealthMember `json:"members"`
				} `json:"cluster"`
			} `json:"checks"`
		}
		if err := json.Unmarshal(raw, &body); err != nil || body.Checks.Cluster == nil {
			return nil, fmt.Errorf("%s /health (status %d) has no cluster check: %v", m.Name, status, err)
		}
		out = append(out, clusterHealthView{Name: m.Name, Status: body.Checks.Cluster.Status, Members: body.Checks.Cluster.Members})
	}
	return out, nil
}

// verifyStaleRemoval is the second runner phase of a disk-loss case.
func verifyStaleRemoval(t *testing.T, caseName, stage string) {
	t.Helper()
	var plan staleRemovalPlan
	if !readJSON(t, removalPlanFile(stage), &plan) || plan.LifecycleID != mustEnv(t, "CAESIUM_LIFECYCLE_ID") {
		blockf(t, caseName, "removal plan %s missing or foreign: the replacement never settled", removalPlanFile(stage))
	}
	// UseNumber keeps the carried-over 64-bit node IDs exact; a plain decode
	// into any would round them through float64.
	evidence := map[string]any{}
	if len(plan.Evidence) > 0 {
		dec := json.NewDecoder(bytes.NewReader(plan.Evidence))
		dec.UseNumber()
		if err := dec.Decode(&evidence); err != nil {
			blockf(t, caseName, "first-phase evidence in %s is unreadable: %v", removalPlanFile(stage), err)
		}
	}
	evidence["removal_plan"] = map[string]any{"executor": plan.Executor, "leader_control": plan.Leader,
		"voter_control": plan.Voter, "stale": plan.Stale}
	var host staleRemovalHost
	if !readJSON(t, removalHostFile(stage), &host) || host.LifecycleID != plan.LifecycleID || host.Executor != plan.Executor {
		blockf(t, caseName, "the host never recorded running `caesium system nodes remove` (%s)", removalHostFile(stage))
	}
	evidence["removal_attempts"] = host.Attempts
	if blocked, failed := judgeRemovalAttempts(plan, host); blocked != "" {
		writeCase(t, caseRecord{Name: caseName, Status: statusBlocked, Detail: blocked, Observations: evidence})
		t.Fatalf("BLOCKED %s: %s", caseName, blocked)
	} else if failed != "" {
		writeCase(t, caseRecord{Name: caseName, Status: statusFail, Detail: failed, Observations: evidence})
		t.Fatalf("FAIL %s: %s", caseName, failed)
	}

	_, h, topo := clusterKube(t)
	liveAddrs := make([]string, 0, len(topo.Members))
	for _, m := range topo.Members {
		liveAddrs = append(liveAddrs, m.DqliteAddr())
	}
	sort.Strings(liveAddrs)
	if strings.Join(liveAddrs, ",") != strings.Join(plan.LiveAddrs, ",") {
		blockf(t, caseName, "live members moved between the phases: planned %v, now %v", plan.LiveAddrs, liveAddrs)
	}
	var staleIDs []uint64
	for _, raw := range plan.StaleIDs {
		id, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			blockf(t, caseName, "unreadable stale id %q in the plan", raw)
		}
		staleIDs = append(staleIDs, id)
	}

	// Followers apply the configuration change as it replicates and /health
	// serves a liveness snapshot refreshed every 10 s: poll a bounded window
	// and judge the last complete observation.
	var views []ordinalZeroView
	var health []clusterHealthView
	var removedErr, readErr error
	deadline := time.Now().Add(2 * time.Minute)
	for {
		views, readErr = directViews(t, topo)
		if readErr == nil {
			health, readErr = clusterHealthViews(t, h, topo)
		}
		if readErr == nil {
			if removedErr = staleEntriesRemoved(views, health, liveAddrs, staleIDs); removedErr == nil {
				break
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(5 * time.Second)
	}
	evidence["post_removal_direct_cluster_views"] = views
	evidence["post_removal_health_views"] = health
	if readErr != nil {
		writeCase(t, caseRecord{Name: caseName, Status: statusBlocked,
			Detail: "post-removal membership unobservable after 2m: " + readErr.Error(), Observations: evidence})
		t.Fatalf("BLOCKED %s: %v", caseName, readErr)
	}
	if removedErr != nil {
		writeCase(t, caseRecord{Name: caseName, Status: statusFail, Detail: "after removal: " + removedErr.Error(), Observations: evidence})
		t.Fatalf("FAIL %s: %v", caseName, removedErr)
	}

	// The node list an operator reads must not name a removed ID either.
	stale := map[string]bool{}
	for _, raw := range plan.StaleIDs {
		stale[raw] = true
	}
	for _, m := range topo.Members {
		raw, err := h.SystemNodes(t.Context(), m.HTTPBase())
		if err != nil {
			blockf(t, caseName, "GET /v1/system/nodes via %s: %v", m.Name, err)
		}
		var nodes []struct {
			ID      string `json:"id"`
			Address string `json:"address"`
		}
		if err := json.Unmarshal(raw, &nodes); err != nil {
			blockf(t, caseName, "GET /v1/system/nodes via %s is not JSON: %v", m.Name, err)
		}
		for _, n := range nodes {
			if stale[n.ID] {
				failClusterCase(t, caseName, "%s still lists removed member %s at %s in /v1/system/nodes", m.Name, n.ID, n.Address)
			}
		}
		evidence["post_removal_system_nodes_"+m.Name] = json.RawMessage(raw)
	}

	removed := make([]string, 0, len(plan.Stale))
	for _, s := range plan.Stale {
		removed = append(removed, fmt.Sprintf("%s/%s/%s", s.ID, s.Address, s.Role))
	}
	writeCase(t, caseRecord{Name: caseName, Status: statusPass,
		Detail: fmt.Sprintf("%s; `caesium system nodes remove` on %s refused the leader %s (leader) and voter %s (voter), "+
			"then removed the stale entries %v; they are gone from every member's direct Cluster RPC and /health view, "+
			"which list exactly the three live voters", plan.Detail, plan.Executor, plan.Leader.ID, plan.Voter.ID, removed),
		Observations: evidence})
}

// TestLifecycleClusterJoiningOrdinalOneRemoved is the ordinal-1 case's second
// phase.
func TestLifecycleClusterJoiningOrdinalOneRemoved(t *testing.T) {
	verifyStaleRemoval(t, "joining-ordinal-1-replacement", "ordinal1")
}

// TestLifecycleClusterOrdinalZeroRemoved is the ordinal-0 case's second phase.
func TestLifecycleClusterOrdinalZeroRemoved(t *testing.T) {
	verifyStaleRemoval(t, "ordinal-0-disk-loss", "ordinal0")
}
