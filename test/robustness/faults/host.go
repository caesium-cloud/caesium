//go:build integration

package faults

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/caesium-cloud/caesium/test/robustness/cluster"
	"github.com/google/uuid"
	"k8s.io/client-go/kubernetes"
)

// Evidence section markers shared with scripts/robustness.sh. The host
// controller returns raw command output; the runner parses it here rather than
// trusting a boolean the script computed.
const (
	EvidenceSrcMarker = "=== src "
	EvidenceDstMarker = "=== dst "
	EvidenceEnd       = " ==="
)

// HostController issues the B2 fault actions to the owned host controller.
//
// All node-level process and network control stays with the host controller,
// as A1 requires: the in-cluster runner never execs a kind node itself. The
// runner asks, the controller acts and returns its raw evidence, and the
// assertions below run on that evidence.
type HostController struct {
	Kube      *kubernetes.Clientset
	Namespace string
}

func (h HostController) request(ctx context.Context, action string, params map[string]string) (cluster.HostAck, error) {
	return cluster.RequestHost(ctx, h.Kube, h.Namespace, cluster.HostRequest{
		RequestID: uuid.NewString(),
		Action:    action,
		Params:    params,
	})
}

// TaskState reads one container's runtime state and returns the raw listing.
func (h HostController) TaskState(ctx context.Context, node, containerID string) (string, string, error) {
	ack, err := h.request(ctx, cluster.ActionTaskState, map[string]string{
		"node": node, "container_id": containerID,
	})
	if err != nil {
		return TaskUnknown, ack.Evidence, err
	}
	status, err := TaskStatus(ack.Evidence, containerID)
	return status, ack.Evidence, err
}

// Pause freezes a real member process externally and returns the raw listing
// the controller observed afterwards.
func (h HostController) Pause(ctx context.Context, node, containerID string) (string, string, error) {
	ack, err := h.request(ctx, cluster.ActionPause, map[string]string{
		"node": node, "container_id": containerID,
	})
	if err != nil {
		return TaskUnknown, ack.Evidence, err
	}
	status, err := TaskStatus(ack.Evidence, containerID)
	return status, ack.Evidence, err
}

// Resume unfreezes it.
func (h HostController) Resume(ctx context.Context, node, containerID string) (string, string, error) {
	ack, err := h.request(ctx, cluster.ActionResume, map[string]string{
		"node": node, "container_id": containerID,
	})
	if err != nil {
		return TaskUnknown, ack.Evidence, err
	}
	status, err := TaskStatus(ack.Evidence, containerID)
	return status, ack.Evidence, err
}

// ProbeResult is one reachability probe run INSIDE a member's own namespaces,
// so the source of the connection really is the partitioned peer.
type ProbeResult struct {
	From    string
	Target  string
	Code    int
	Output  string
	Success bool
}

// Err renders the failure for evidence, or "" when the probe succeeded.
func (p ProbeResult) Err() string {
	if p.Success {
		return ""
	}
	return fmt.Sprintf("probe from %s to %s exited %d: %s", p.From, p.Target, p.Code, strings.TrimSpace(p.Output))
}

// Probe runs a bounded HTTP fetch from inside containerID's namespaces.
func (h HostController) Probe(ctx context.Context, node, containerID, from, target string, timeoutSec int) (ProbeResult, error) {
	ack, err := h.request(ctx, "exec-probe", map[string]string{
		"node": node, "container_id": containerID,
		"target": target, "timeout_s": strconv.Itoa(timeoutSec),
	})
	res := ProbeResult{From: from, Target: target, Output: ack.Evidence, Code: -1}
	if err != nil {
		return res, err
	}
	code, body, perr := parseProbeEvidence(ack.Evidence)
	if perr != nil {
		return res, perr
	}
	res.Code = code
	res.Output = body
	res.Success = code == 0
	return res, nil
}

func parseProbeEvidence(evidence string) (int, string, error) {
	lines := strings.SplitN(strings.TrimLeft(evidence, "\n"), "\n", 2)
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "rc=") {
		return -1, evidence, fmt.Errorf("probe evidence has no rc= line: %q", truncate(evidence, 400))
	}
	code, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(lines[0], "rc=")))
	if err != nil {
		return -1, evidence, fmt.Errorf("probe evidence rc is not a number: %q", lines[0])
	}
	body := ""
	if len(lines) > 1 {
		body = lines[1]
	}
	return code, body, nil
}

// Partition installs the asymmetric network rules and returns the raw counter
// listings from both nodes.
func (h HostController) Partition(ctx context.Context, p PartitionPlan) (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	ack, err := h.request(ctx, cluster.ActionPartition, p.params())
	return ack.Evidence, err
}

// Counters re-reads both directions' rule counters.
func (h HostController) Counters(ctx context.Context, p PartitionPlan) (blocked, open map[string]Counter, raw string, err error) {
	ack, err := h.request(ctx, cluster.ActionCounters, p.params())
	if err != nil {
		return nil, nil, ack.Evidence, err
	}
	b, o := SplitCounterEvidence(ack.Evidence)
	return b, o, ack.Evidence, nil
}

// Heal removes exactly the rules this plan installed and returns the final
// counters plus proof that no tagged rule remains.
func (h HostController) Heal(ctx context.Context, p PartitionPlan) (string, error) {
	ack, err := h.request(ctx, cluster.ActionHeal, p.params())
	return ack.Evidence, err
}

func (p PartitionPlan) params() map[string]string {
	return map[string]string{
		"tag":         p.Tag,
		"src_node":    p.SrcNode,
		"dst_node":    p.DstNode,
		"src_ip":      p.SrcIP,
		"dst_ip":      p.DstIP,
		"drop_ports":  joinPorts(p.DropPorts),
		"count_ports": joinPorts(p.CountPorts),
	}
}

func joinPorts(ports []int) string {
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, strconv.Itoa(p))
	}
	return strings.Join(parts, ",")
}

// SplitCounterEvidence separates the src-node and dst-node sections of a
// counter evidence blob.
func SplitCounterEvidence(evidence string) (blocked, open map[string]Counter) {
	var src, dst []string
	section := ""
	for _, line := range strings.Split(evidence, "\n") {
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, EvidenceSrcMarker):
			section = "src"
			continue
		case strings.HasPrefix(lower, EvidenceDstMarker):
			section = "dst"
			continue
		}
		switch section {
		case "src":
			src = append(src, line)
		case "dst":
			dst = append(dst, line)
		}
	}
	return ParseCounters(strings.Join(src, "\n")), ParseCounters(strings.Join(dst, "\n"))
}

// ArmBusPublishPause writes the test-only directive into a member's control
// directory. It is a plain file on the pod's own emptyDir, written through the
// host controller's container runtime: no product endpoint, no listener and no
// new RBAC are involved.
func (h HostController) ArmBusPublishPause(ctx context.Context, node, containerID, dir, directiveJSON string) (string, error) {
	ack, err := h.request(ctx, "testfault-arm", map[string]string{
		"node": node, "container_id": containerID,
		"dir": dir, "directive": directiveJSON,
	})
	return ack.Evidence, err
}

// DisarmBusPublishPause removes the directive, releasing any held publication.
func (h HostController) DisarmBusPublishPause(ctx context.Context, node, containerID, dir string) (string, error) {
	ack, err := h.request(ctx, "testfault-disarm", map[string]string{
		"node": node, "container_id": containerID, "dir": dir,
	})
	return ack.Evidence, err
}

// BusPublishHookLog reads a member's append-only hook record.
func (h HostController) BusPublishHookLog(ctx context.Context, node, containerID, dir string) (string, error) {
	ack, err := h.request(ctx, "testfault-log", map[string]string{
		"node": node, "container_id": containerID, "dir": dir,
	})
	return ack.Evidence, err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
