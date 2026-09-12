//go:build !integration

package robustness

import "testing"

func TestKillEvidenceShowsDeathRejectsTransportErrors(t *testing.T) {
	cid := "abcdef1234567890deadbeefcafebabe"
	evidence := "kubelet stopped on worker\nctr kill " + cid + "\nctr: failed to dial containerd: connection refused\n"
	if killEvidenceShowsDeath(evidence, cid) {
		t.Fatalf("connection refused listing must not count as death")
	}
}

func TestKillEvidenceShowsDeathRequiresValidListing(t *testing.T) {
	cid := "abcdef1234567890deadbeefcafebabe"
	header := "kubelet stopped on worker\nctr kill " + cid + "\n"
	if killEvidenceShowsDeath(header, cid) {
		t.Fatalf("CID in ctr kill header is not death evidence")
	}
	running := header + "TASK PID STATUS\n" + cid + " 1 RUNNING\n"
	if killEvidenceShowsDeath(running, cid) {
		t.Fatalf("running task is not death")
	}
	stopped := header + "TASK PID STATUS\n" + cid[:12] + " 1 STOPPED\n"
	if !killEvidenceShowsDeath(stopped, cid) {
		t.Fatalf("stopped listing should be death")
	}
	gone := header + "TASK                                PID      STATUS\n"
	if !killEvidenceShowsDeath(gone, cid) {
		t.Fatalf("valid listing without CID should be death")
	}
}

func TestTaskDeadFromListing(t *testing.T) {
	cid := "abcdef1234567890deadbeefcafebabe"
	if dead, _ := taskDeadFromListing(cid, "ctr: failed to dial containerd: connection refused", true); dead {
		t.Fatalf("seen=1 plus dial error must not be death")
	}
	listing := "TASK PID STATUS\n"
	if dead, _ := taskDeadFromListing(cid, listing, false); dead {
		t.Fatalf("absent without prior observation is not death")
	}
	if dead, _ := taskDeadFromListing(cid, listing, true); !dead {
		t.Fatalf("absent after seen on valid listing is death")
	}
}
