package faults

import (
	"strings"
	"testing"
)

const sampleForward = `Chain FORWARD (policy ACCEPT 0 packets, 0 bytes)
    pkts      bytes target     prot opt in     out     source               destination
      41       2460 DROP       tcp  --  *      *       10.244.1.5           10.244.2.7           tcp dpt:9001 /* rb-tag-drop-9001 */
       0          0 DROP       tcp  --  *      *       10.244.1.5           10.244.2.7           tcp dpt:8443 /* rb-tag-drop-8443 */
      17       1020 RETURN     tcp  --  *      *       10.244.2.7           10.244.1.5           tcp dpt:9001 /* rb-tag-open-count-9001 */
    1032      61920 KUBE-FORWARD  all  --  *      *       0.0.0.0/0            0.0.0.0/0
`

func TestParseCountersKeepsPerRuleTraffic(t *testing.T) {
	got := ParseCounters(sampleForward)
	if len(got) != 3 {
		t.Fatalf("expected 3 commented rules, got %d: %v", len(got), got)
	}
	if c := got["rb-tag-drop-9001"]; c.Packets != 41 || c.Bytes != 2460 {
		t.Fatalf("raft drop counter misparsed: %+v", c)
	}
	if c := got["rb-tag-drop-8443"]; c.Packets != 0 {
		t.Fatalf("zero counter misparsed: %+v", c)
	}
	if c := got["rb-tag-open-count-9001"]; c.Packets != 17 {
		t.Fatalf("open counter misparsed: %+v", c)
	}
}

func TestParseCountersIgnoresUncommentedRules(t *testing.T) {
	if _, ok := ParseCounters(sampleForward)["KUBE-FORWARD"]; ok {
		t.Fatal("an unrelated chain jump was parsed as one of our rules")
	}
}

func TestParseCountersOnGarbage(t *testing.T) {
	if got := ParseCounters("iptables: command not found"); len(got) != 0 {
		t.Fatalf("garbage produced counters: %v", got)
	}
}

func plan() PartitionPlan {
	return PartitionPlan{
		Tag:     "rb-tag",
		SrcPod:  "caesium-0",
		DstPod:  "caesium-1",
		SrcIP:   "10.244.1.5",
		DstIP:   "10.244.2.7",
		SrcNode: "rb-worker",
		DstNode: "rb-worker2",
		// The API port makes the directional probe observable; the two peer
		// ports are the discovered Raft and dispatch routes.
		DropPorts:  []int{PortAPI, PortInternal, PortRaft},
		CountPorts: []int{PortRaft, PortInternal},
	}
}

func TestPlanValidates(t *testing.T) {
	if err := plan().Validate(); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
}

func TestPlanRejectsSameNodeEndpoints(t *testing.T) {
	p := plan()
	p.DstNode = p.SrcNode
	if err := p.Validate(); err == nil {
		t.Fatal("a plan whose endpoints share a node is not directional and must be rejected")
	}
}

func TestPlanRejectsSameAddress(t *testing.T) {
	p := plan()
	p.DstIP = p.SrcIP
	if err := p.Validate(); err == nil {
		t.Fatal("a plan with one address must be rejected")
	}
}

func TestBlockedRulesDropEveryPartitionedPort(t *testing.T) {
	rules := plan().BlockedRules()
	if len(rules) != 3 {
		t.Fatalf("expected one drop per partitioned port: %+v", rules)
	}
	for _, r := range rules {
		if r.Target != "DROP" || r.Chain != "FORWARD" {
			t.Fatalf("blocked rule is not a FORWARD drop: %+v", r)
		}
		if r.SrcIP != "10.244.1.5" || r.DstIP != "10.244.2.7" {
			t.Fatalf("blocked rule is not directional: %+v", r)
		}
	}
}

// The open direction must be counted without being changed: a RETURN hands the
// packet back to FORWARD at the rule after the jump.
func TestOpenDirectionIsCountedButNotAltered(t *testing.T) {
	p := plan()
	rules := p.OpenRules()
	if len(rules) != 2 {
		t.Fatalf("expected one counter per counted port: %+v", rules)
	}
	for _, r := range rules {
		if r.Target != "RETURN" {
			t.Fatalf("the open direction must not be dropped or accepted early: %+v", r)
		}
		if r.Chain != p.OpenChain() {
			t.Fatalf("open counters must live in this run's own chain: %+v", r)
		}
		if r.SrcIP != p.DstIP || r.DstIP != p.SrcIP {
			t.Fatalf("open counter is not the reverse direction: %+v", r)
		}
	}
}

func TestOpenChainIsDerivedFromTheTag(t *testing.T) {
	chain := plan().OpenChain()
	if chain != "CSRBTAG" {
		t.Fatalf("unexpected chain name %q", chain)
	}
	if len(chain) > 28 {
		t.Fatalf("chain name %q exceeds the iptables limit", chain)
	}
}

func TestPlanRejectsUncountedRaftRoute(t *testing.T) {
	p := plan()
	p.CountPorts = []int{PortInternal}
	if err := p.Validate(); err == nil {
		t.Fatal("a plan that does not count the discovered Raft route must be rejected")
	}
}

func TestPlanRejectsCountingAnUndroppedPort(t *testing.T) {
	p := plan()
	p.DropPorts = []int{PortRaft}
	p.CountPorts = []int{PortRaft, PortAPI}
	if err := p.Validate(); err == nil {
		t.Fatal("counting a port that is not dropped would not evidence the blocked route")
	}
}

func TestRuleArgsCarryTheDiscoveredAddress(t *testing.T) {
	args := strings.Join(RuleSpec{
		Chain: "FORWARD", SrcIP: "10.0.0.1", DstIP: "10.0.0.2", Proto: "tcp",
		DPort: PortRaft, Target: "DROP", Comment: "rb-x-drop-9001",
	}.Args("-I", 1), " ")
	for _, want := range []string{"-I FORWARD 1", "-s 10.0.0.1", "-d 10.0.0.2", "--dport 9001", "-j DROP", "--comment rb-x-drop-9001"} {
		if !strings.Contains(args, want) {
			t.Fatalf("rendered rule %q is missing %q", args, want)
		}
	}
}

func TestRuleValidationRejectsUnidentifiableRules(t *testing.T) {
	if err := (RuleSpec{Chain: "FORWARD", DstIP: "10.0.0.2", Proto: "tcp", DPort: 9001}).Validate(); err == nil {
		t.Fatal("a rule with no comment cannot be healed and must be rejected")
	}
	if err := (RuleSpec{Chain: "FORWARD", DstIP: "10.0.0.2", Proto: "tcp", DPort: 9001, Comment: `a"b`}).Validate(); err == nil {
		t.Fatal("a comment that breaks iptables parsing must be rejected")
	}
}

func TestActivationNeedsTrafficThroughTheInjector(t *testing.T) {
	p := plan()
	// A rule that matched no packets means the discovered peer address never
	// traversed the injector, whatever the probes said.
	err := p.Activated(ActivationEvidence{
		BlockedCounters: map[string]Counter{"rb-tag-drop-9001": {Packets: 0}},
		BlockedProbeErr: "dial timeout",
		OpenProbeOK:     true,
		OpenProbeFrom:   "caesium-2",
	})
	if err == nil {
		t.Fatal("a zero packet counter must not count as an activated partition")
	}
	if !strings.Contains(err.Error(), "does not traverse the injector") {
		t.Fatalf("unexpected reason: %v", err)
	}
}

func TestActivationNeedsAnObservedBlock(t *testing.T) {
	p := plan()
	err := p.Activated(ActivationEvidence{
		BlockedCounters: map[string]Counter{"rb-tag-drop-9001": {Packets: 12}},
		BlockedProbeErr: "",
		OpenProbeOK:     true,
		OpenProbeFrom:   "caesium-2",
	})
	if err == nil {
		t.Fatal("a reachable blocked direction must not count as a partition")
	}
}

func TestActivationRejectsGeneralOutage(t *testing.T) {
	p := plan()
	err := p.Activated(ActivationEvidence{
		BlockedCounters: map[string]Counter{"rb-tag-drop-9001": {Packets: 12}},
		BlockedProbeErr: "dial timeout",
		OpenProbeOK:     false,
		OpenProbeFrom:   "caesium-2",
	})
	if err == nil {
		t.Fatal("a destination unreachable from an unpartitioned peer is a general outage, not a partition")
	}
}

func TestActivationAcceptsRealEvidence(t *testing.T) {
	p := plan()
	if err := p.Activated(ActivationEvidence{
		BlockedCounters: map[string]Counter{"rb-tag-drop-9001": {Packets: 41}},
		BlockedProbeErr: "dial tcp 10.244.2.7:8080: i/o timeout",
		OpenProbeOK:     true,
		OpenProbeFrom:   "caesium-2",
	}); err != nil {
		t.Fatalf("real activation evidence rejected: %v", err)
	}
}

func TestHealNeedsBothDirections(t *testing.T) {
	p := plan()
	if err := p.Healed(false, true); err == nil {
		t.Fatal("heal must require the previously blocked direction to recover")
	}
	if err := p.Healed(true, true); err != nil {
		t.Fatalf("healed evidence rejected: %v", err)
	}
}

func TestDeleteArgsOnlyTargetOurRules(t *testing.T) {
	save := `-P FORWARD ACCEPT
-A FORWARD -s 10.244.1.5/32 -d 10.244.2.7/32 -p tcp -m comment --comment "rb-tag-drop-9001" -m tcp --dport 9001 -j DROP
-A FORWARD -j KUBE-FORWARD
-A FORWARD -s 10.244.9.9/32 -m comment --comment "someone-elses-rule" -j DROP
`
	got := DeleteArgsFrom(save, "rb-tag")
	if len(got) != 1 {
		t.Fatalf("expected exactly our rule, got %v", got)
	}
	joined := strings.Join(got[0], " ")
	if !strings.HasPrefix(joined, "-D FORWARD") || !strings.Contains(joined, "rb-tag-drop-9001") {
		t.Fatalf("delete args are wrong: %q", joined)
	}
	if strings.Contains(joined, `"`) {
		t.Fatalf("comment quoting leaked into the delete args: %q", joined)
	}
}

func TestDeleteArgsIgnoreUnrelatedTags(t *testing.T) {
	if got := DeleteArgsFrom(`-A FORWARD -m comment --comment "other" -j DROP`, "rb-tag"); len(got) != 0 {
		t.Fatalf("heal would have deleted a foreign rule: %v", got)
	}
}
