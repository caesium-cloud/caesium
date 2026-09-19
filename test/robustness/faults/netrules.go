// Package faults holds the targeted fault controls for the robustness harness.
//
// Three distinct mechanisms live here, and none substitutes for another:
//
//   - netrules.go builds and reads EXTERNAL network fault rules applied inside
//     the owned kind node containers. A1 forbade adopting a proxy without a
//     live routing spike, because the chart advertises
//     CAESIUM_NODE_ADDRESS=$(POD_IP):9001 to dqlite and dispatch derives
//     https://podIP:8443 from that same advertised address — so replacing seed
//     lists never establishes that the real Raft or dispatch routes cross an
//     injector. Rules keyed by the DISCOVERED peer address, with their own
//     packet counters, do establish it.
//   - pause.go reads external process pause/resume state.
//   - interposer.go is a CLIENT-side response fault, on the recorder/runner's
//     own route, so "commit before response loss" is observable and the
//     operation can be retained as possibly committed.
package faults

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Ports the robustness harness partitions. They are the ports the discovered
// peer addresses actually resolve to: 9001 is the dqlite/Raft membership port
// advertised as CAESIUM_NODE_ADDRESS, 8443 is the dedicated internal mTLS
// dispatch listener derived from it, and 8080 is the public API used for the
// directional reachability probe.
const (
	PortRaft     = 9001
	PortInternal = 8443
	PortAPI      = 8080
)

// RuleSpec is one iptables rule in the filter table.
//
// Every rule carries its own packet counter, which is the independent
// activation evidence. Blocking rules use DROP in the FORWARD chain of the
// node hosting the source. Counting rules for the direction that must STAY UP
// use RETURN inside a dedicated user chain jumped from FORWARD: a RETURN hands
// the packet back to FORWARD at the rule after the jump, so the open direction
// is counted without changing what happens to it.
type RuleSpec struct {
	Chain   string
	SrcIP   string
	DstIP   string
	Proto   string
	DPort   int
	Target  string
	Comment string
}

// Validate rejects a rule the harness would not be able to identify or heal.
func (r RuleSpec) Validate() error {
	if r.Chain == "" {
		return fmt.Errorf("rule needs a chain")
	}
	if r.DstIP == "" {
		return fmt.Errorf("rule needs a destination address")
	}
	if r.Comment == "" {
		return fmt.Errorf("rule needs a comment so heal can find and delete it")
	}
	if strings.ContainsAny(r.Comment, `"'*/\`) {
		return fmt.Errorf("rule comment %q contains a character that breaks iptables parsing", r.Comment)
	}
	if r.DPort <= 0 || r.DPort > 65535 {
		return fmt.Errorf("rule needs a destination port, got %d", r.DPort)
	}
	if r.Proto == "" {
		return fmt.Errorf("rule needs a protocol")
	}
	return nil
}

// Args renders the rule for an iptables operation ("-I", "-A", "-D", "-C").
// Insert positions are supplied by the caller through pos (0 = append order).
func (r RuleSpec) Args(op string, pos int) []string {
	args := []string{op, r.Chain}
	if op == "-I" && pos > 0 {
		args = append(args, strconv.Itoa(pos))
	}
	if r.SrcIP != "" {
		args = append(args, "-s", r.SrcIP)
	}
	args = append(args, "-d", r.DstIP, "-p", r.Proto, "--dport", strconv.Itoa(r.DPort))
	args = append(args, "-m", "comment", "--comment", r.Comment)
	if r.Target != "" {
		args = append(args, "-j", r.Target)
	}
	return args
}

// Counter is one rule's observed traffic.
type Counter struct {
	Comment string
	Packets uint64
	Bytes   uint64
}

var counterLine = regexp.MustCompile(`^\s*(\d+)\s+(\d+)\s+(\S*)\s`)
var commentRe = regexp.MustCompile(`/\*\s*([^*]+?)\s*\*/`)

// ParseCounters reads `iptables -t filter -L <chain> -v -n -x` output and
// returns the packet/byte counters of every rule that carries a comment.
//
// Counters are the independent activation evidence: a fault that never matched
// a packet was not activated, whatever the control command returned.
func ParseCounters(text string) map[string]Counter {
	out := map[string]Counter{}
	for _, line := range strings.Split(text, "\n") {
		m := commentRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		fields := counterLine.FindStringSubmatch(line)
		if fields == nil {
			continue
		}
		pkts, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		bytes, err := strconv.ParseUint(fields[2], 10, 64)
		if err != nil {
			continue
		}
		comment := strings.TrimSpace(m[1])
		out[comment] = Counter{Comment: comment, Packets: pkts, Bytes: bytes}
	}
	return out
}

// PartitionPlan is one asymmetric partition: traffic from Src toward Dst's
// discovered address is dropped, while Dst toward Src stays open.
type PartitionPlan struct {
	Tag string
	// SrcPod/DstPod are the pod names, for the record only.
	SrcPod, DstPod string
	SrcIP, DstIP   string
	// SrcNode is the kind node the blocking rules are installed on: the node
	// hosting Src, so only that direction is affected.
	SrcNode string
	// DstNode hosts the open direction's counter rules.
	DstNode string
	// DropPorts are dropped from Src to Dst.
	DropPorts []int
	// CountPorts are the discovered peer routes whose traversal is asserted or
	// reported. They must be a subset of DropPorts, because the blocked
	// direction's evidence is the DROP rules' own packet counters.
	CountPorts []int
}

// DropRuleComment is the identity of the blocking rule for one port. Its packet
// counter is the traversal evidence for that discovered route.
func (p PartitionPlan) DropRuleComment(port int) string {
	return fmt.Sprintf("%s-drop-%d", p.Tag, port)
}

// OpenRuleComment is the identity of the open direction's counting rule.
func (p PartitionPlan) OpenRuleComment(port int) string {
	return fmt.Sprintf("%s-open-count-%d", p.Tag, port)
}

// BlockedRules are the DROP rules installed in SrcNode's FORWARD chain.
func (p PartitionPlan) BlockedRules() []RuleSpec {
	out := make([]RuleSpec, 0, len(p.DropPorts))
	for _, port := range p.DropPorts {
		out = append(out, RuleSpec{
			Chain: "FORWARD", SrcIP: p.SrcIP, DstIP: p.DstIP, Proto: "tcp", DPort: port,
			Target: "DROP", Comment: p.DropRuleComment(port),
		})
	}
	return out
}

// OpenRules count the direction that must stay up, inside OpenChain, and hand
// the packet straight back to FORWARD.
func (p PartitionPlan) OpenRules() []RuleSpec {
	out := make([]RuleSpec, 0, len(p.CountPorts))
	for _, port := range p.CountPorts {
		out = append(out, RuleSpec{
			Chain: p.OpenChain(), SrcIP: p.DstIP, DstIP: p.SrcIP, Proto: "tcp", DPort: port,
			Target: "RETURN", Comment: p.OpenRuleComment(port),
		})
	}
	return out
}

// OpenChain is the user chain holding the open direction's counters. It is
// derived from the tag so heal can find and remove exactly this run's chain.
func (p PartitionPlan) OpenChain() string {
	var b strings.Builder
	b.WriteString("CS")
	for _, r := range strings.ToUpper(p.Tag) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
		if b.Len() >= 20 {
			break
		}
	}
	return b.String()
}

// Validate checks the plan is well formed and genuinely asymmetric.
func (p PartitionPlan) Validate() error {
	if p.Tag == "" {
		return fmt.Errorf("partition plan needs a tag")
	}
	if p.SrcIP == "" || p.DstIP == "" {
		return fmt.Errorf("partition plan needs both discovered addresses")
	}
	if p.SrcIP == p.DstIP {
		return fmt.Errorf("partition plan source and destination are the same address %s", p.SrcIP)
	}
	if p.SrcNode == "" || p.DstNode == "" {
		return fmt.Errorf("partition plan needs both hosting nodes")
	}
	if p.SrcNode == p.DstNode {
		return fmt.Errorf("partition plan endpoints share node %s, so the rule is not directional", p.SrcNode)
	}
	if len(p.DropPorts) == 0 {
		return fmt.Errorf("partition plan drops nothing")
	}
	for _, port := range p.CountPorts {
		if !containsInt(p.DropPorts, port) {
			return fmt.Errorf("counted port %d is not dropped, so its rule counter would not be the blocked route's evidence", port)
		}
	}
	if !containsInt(p.CountPorts, PortRaft) {
		return fmt.Errorf("partition plan does not count the discovered Raft port %d", PortRaft)
	}
	for _, r := range append(p.BlockedRules(), p.OpenRules()...) {
		if err := r.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func containsInt(list []int, want int) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// ActivationEvidence decides whether an asymmetric partition was really active.
//
// Every clause must hold independently: the blocked direction's rule matched
// real packets, the blocked reachability probe failed, and the open direction's
// probe succeeded. A control command that returned success proves nothing on
// its own, and a failing probe with a zero counter is a broken probe, not a
// partition.
type ActivationEvidence struct {
	BlockedCounters map[string]Counter
	OpenCounters    map[string]Counter
	// BlockedProbeErr is the failure observed from INSIDE the partitioned
	// source's own namespaces, toward the destination's discovered address.
	BlockedProbeErr string
	// OpenProbeOK reports that the destination still answers on the same port
	// from an UNPARTITIONED peer. That is what distinguishes an asymmetric
	// partition from a general outage of the destination.
	//
	// The destination->source direction is recorded separately in
	// ReverseProbeErr and deliberately NOT asserted: the partitioned member is
	// cut off from the Raft leader, so its own quorum-dependent API degrades as
	// a CONSEQUENCE of the fault. Requiring it to answer would make a correct
	// partition look like a broken one.
	OpenProbeOK     bool
	OpenProbeFrom   string
	ReverseProbeErr string
}

// Activated returns nil when the evidence supports the claim, or the reason it
// does not.
func (p PartitionPlan) Activated(e ActivationEvidence) error {
	raftBlocked := p.DropRuleComment(PortRaft)
	c, ok := e.BlockedCounters[raftBlocked]
	if !ok {
		return fmt.Errorf("no counter rule %s was observed: the discovered Raft address never traversed the injector", raftBlocked)
	}
	if c.Packets == 0 {
		return fmt.Errorf("counter rule %s matched 0 packets: the discovered peer address does not traverse the injector", raftBlocked)
	}
	if e.BlockedProbeErr == "" {
		return fmt.Errorf("the blocked direction %s->%s still reached its peer", p.SrcPod, p.DstPod)
	}
	if !e.OpenProbeOK {
		return fmt.Errorf("%s is unreachable from the unpartitioned peer %s as well: this is a general outage of %s, not an asymmetric partition",
			p.DstPod, e.OpenProbeFrom, p.DstPod)
	}
	return nil
}

// Healed returns nil when both directions are reachable again.
func (p PartitionPlan) Healed(blockedProbeOK, openProbeOK bool) error {
	if !blockedProbeOK {
		return fmt.Errorf("previously blocked direction %s->%s did not recover after heal", p.SrcPod, p.DstPod)
	}
	if !openProbeOK {
		return fmt.Errorf("open direction %s->%s broke during heal", p.DstPod, p.SrcPod)
	}
	return nil
}

// DeleteArgsFrom turns `iptables -S <chain>` output into the delete commands
// for every rule carrying the tag. Healing by the saved rule spec is what makes
// cleanup exact: it can never remove a rule the harness did not install.
func DeleteArgsFrom(saveOutput, tag string) [][]string {
	var out [][]string
	for _, line := range strings.Split(saveOutput, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "-A ") || !strings.Contains(line, tag) {
			continue
		}
		fields, err := splitRuleLine(strings.TrimPrefix(line, "-A "))
		if err != nil || len(fields) == 0 {
			continue
		}
		out = append(out, append([]string{"-D"}, fields...))
	}
	return out
}

// splitRuleLine splits an `iptables -S` rule body, honouring the quoted comment.
func splitRuleLine(line string) ([]string, error) {
	var (
		fields  []string
		current strings.Builder
		quoted  bool
		has     bool
	)
	for _, r := range line {
		switch {
		case r == '"':
			quoted = !quoted
			has = true
		case r == ' ' && !quoted:
			if has {
				fields = append(fields, current.String())
				current.Reset()
				has = false
			}
		default:
			current.WriteRune(r)
			has = true
		}
	}
	if has {
		fields = append(fields, current.String())
	}
	if quoted {
		return nil, fmt.Errorf("unterminated quote in rule %q", line)
	}
	return fields, nil
}
