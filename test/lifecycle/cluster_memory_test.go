//go:build integration

package lifecycle

// #583 (distributed-testing H4, W9-δ): the snapshot catch-up soak can place
// dqlite leadership on a chosen survivor before its catalog writes, so the
// leader-only native residual is measured with the Raft leader co-located with
// the harness's HTTP write target (caesium-0) and separated from it
// (caesium-1). Leadership moves through go-dqlite's Transfer RPC on the
// current leader; nothing restarts, and the stopped caesium-2 stays a voter.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/test/robustness/cluster"
	dqclient "github.com/canonical/go-dqlite/v3/client"
)

type pinLeaderRecord struct {
	LifecycleID string    `json:"lifecycle_id"`
	Target      string    `json:"target"`
	TargetID    uint64    `json:"target_id"`
	Before      string    `json:"leader_before"`
	After       string    `json:"leader_after"`
	Transferred bool      `json:"transferred"`
	Survivors   []string  `json:"survivors"`
	ObservedAt  time.Time `json:"observed_at"`
}

// survivorLeader asks every surviving member for the leader and requires one
// agreed answer that maps to a surviving member.
func survivorLeader(ctx context.Context, addrs map[string]string) (string, uint64, error) {
	agreed := ""
	var agreedID uint64
	for name, addr := range addrs {
		leader, _, err := cluster.QueryNode(ctx, addr)
		if err != nil {
			return "", 0, err
		}
		if leader == nil || leader.Address == "" {
			return "", 0, fmt.Errorf("%s reports no leader", name)
		}
		if agreed != "" && leader.Address != agreed {
			return "", 0, fmt.Errorf("leader disagreement: %s vs %s", agreed, leader.Address)
		}
		agreed, agreedID = leader.Address, leader.ID
	}
	for name, addr := range addrs {
		if addr == agreed {
			return name, agreedID, nil
		}
	}
	return "", 0, fmt.Errorf("agreed leader %s is not a surviving member", agreed)
}

func TestLifecycleClusterPinLeader(t *testing.T) {
	target := mustEnv(t, "CAESIUM_LIFECYCLE_PIN_LEADER")
	if target != "caesium-0" && target != "caesium-1" {
		t.Fatalf("CAESIUM_LIFECYCLE_PIN_LEADER=%q, want caesium-0 or caesium-1", target)
	}
	lifecycleID := mustEnv(t, "CAESIUM_LIFECYCLE_ID")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	kube, err := cluster.InClusterClient()
	if err != nil {
		t.Fatal(err)
	}
	topo, err := cluster.DiscoverTopology(ctx, kube, lifecycleID)
	if err != nil {
		t.Fatal(err)
	}
	addrs := map[string]string{}
	for _, m := range topo.Members {
		if (m.Name == "caesium-0" || m.Name == "caesium-1") && cluster.PodReady(&m.Pod) {
			addrs[m.Name] = m.DqliteAddr()
		}
	}
	if len(addrs) != 2 {
		t.Fatalf("expected the two Ready survivors caesium-0 and caesium-1, found %v", addrs)
	}
	record := pinLeaderRecord{LifecycleID: lifecycleID, Target: target, Survivors: []string{"caesium-0", "caesium-1"}}
	var before string
	var lastErr error
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		if before, _, lastErr = survivorLeader(ctx, addrs); lastErr == nil {
			break
		}
	}
	if lastErr != nil {
		t.Fatalf("survivors never agreed on a leader: %v", lastErr)
	}
	record.Before = before
	if before != target {
		cli, err := dqclient.New(ctx, addrs[before])
		if err != nil {
			t.Fatalf("connect to leader %s: %v", before, err)
		}
		members, err := cli.Cluster(ctx)
		if err != nil {
			_ = cli.Close()
			t.Fatalf("read membership from %s: %v", before, err)
		}
		for _, member := range members {
			if member.Address == addrs[target] {
				record.TargetID = member.ID
			}
		}
		if record.TargetID == 0 {
			_ = cli.Close()
			t.Fatalf("%s at %s is not in the leader's membership %+v", target, addrs[target], members)
		}
		err = cli.Transfer(ctx, record.TargetID)
		_ = cli.Close()
		if err != nil {
			t.Fatalf("transfer leadership %s -> %s: %v", before, target, err)
		}
		record.Transferred = true
	}
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(time.Second) {
		after, id, err := survivorLeader(ctx, addrs)
		if err == nil && after == target {
			record.After, record.TargetID = after, id
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("leadership did not settle on %s within 1m: leader=%s err=%v", target, after, err)
		}
	}
	record.ObservedAt = time.Now().UTC()
	writeJSON(t, "cluster-pin-leader.json", record)
	t.Logf("dqlite leadership pinned on %s (was %s, transferred=%t)", record.After, record.Before, record.Transferred)
}
