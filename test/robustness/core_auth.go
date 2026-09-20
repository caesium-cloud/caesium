//go:build integration

package robustness

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/test/robustness/cluster"
)

func runWrongToken(t *testing.T, fe *faultEnv) {
	refreshTopo(t, fe)
	ctx := context.Background()
	member := fe.leader
	job, alias := applyFaultFixture(t, fe, member, "auth-wrong", 8)
	run, _, err := fe.httpAPI.TriggerRun(ctx, member.HTTPBase(), job.ID)
	if err != nil {
		t.Fatalf("trigger %s: %v", alias, err)
	}
	leaseCtx, leaseCancel := context.WithTimeout(ctx, 90*time.Second)
	lease, err := cluster.WaitLease(leaseCtx, fe.httpAPI, member.HTTPBase(), run.ID, "")
	leaseCancel()
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	detail := waitRunHasTask(t, ctx, fe, member.HTTPBase(), job.ID, run.ID)
	before := fingerprintRun(t, ctx, fe, member.HTTPBase(), job.ID, run.ID)
	cli := wrongTokenClient(t, fe, member)
	target := cluster.InternalBase(member.IP)

	completeEx := cli.Complete(ctx, target, completePayload(detail, lease, "succeeded", lease.Generation))
	if completeEx.Err != "" {
		t.Fatalf("wrong-token complete transport: %v", completeEx.Err)
	}
	requireHTTPStatus(t, completeEx.Status, http.StatusUnauthorized, completeEx.Body)
	code, _ := ParseRefusal(completeEx.Status, []byte(completeEx.Body))
	if code != RefusalUnauthorized {
		t.Fatalf("wrong-token complete code %q, want unauthorized", code)
	}

	peer := otherMember(fe.topo, member)
	dispatchEx := cli.Dispatch(ctx, cluster.InternalBase(peer.IP), dispatchPayload(detail, lease, peer.NodeAddress))
	if dispatchEx.Err != "" {
		t.Fatalf("wrong-token dispatch transport: %v", dispatchEx.Err)
	}
	requireHTTPStatus(t, dispatchEx.Status, http.StatusUnauthorized, dispatchEx.Body)

	after := fingerprintRun(t, ctx, fe, member.HTTPBase(), job.ID, run.ID)
	requireNoMutation(t, before, after, "wrong-token internal requests")

	writeCoreRecord(t, fe, "wrong_token_internal", map[string]any{
		"run_id":          run.ID,
		"complete_status": completeEx.Status,
		"dispatch_status": dispatchEx.Status,
		"principal":       "bearer",
		"token_class":     "wrong",
		"kind":            cli.Kind,
		"target_complete": member.Name,
		"target_dispatch": peer.Name,
		"state_digest":    digestOf(after),
	})
}

func runInvalidMTLS(t *testing.T, fe *faultEnv) {
	refreshTopo(t, fe)
	ctx := context.Background()
	member := fe.leader
	job, alias := applyFaultFixture(t, fe, member, "auth-mtls", 8)
	run, _, err := fe.httpAPI.TriggerRun(ctx, member.HTTPBase(), job.ID)
	if err != nil {
		t.Fatalf("trigger %s: %v", alias, err)
	}
	leaseCtx, leaseCancel := context.WithTimeout(ctx, 90*time.Second)
	lease, err := cluster.WaitLease(leaseCtx, fe.httpAPI, member.HTTPBase(), run.ID, "")
	leaseCancel()
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	detail := waitRunHasTask(t, ctx, fe, member.HTTPBase(), job.ID, run.ID)
	before := fingerprintRun(t, ctx, fe, member.HTTPBase(), job.ID, run.ID)

	mintCtx, mintCancel := context.WithTimeout(ctx, 30*time.Second)
	cli, err := cluster.InvalidCertClient(mintCtx, fe.httpAPI, member.HTTPBase())
	mintCancel()
	if err != nil {
		t.Fatalf("invalid-cert client: %v", err)
	}
	ex := cli.Complete(ctx, cluster.InternalBase(member.IP), completePayload(detail, lease, "succeeded", lease.Generation))
	if ex.Err == "" && ex.Status != 0 {
		t.Fatalf("invalid peer cert reached the handler (status %d body %s); handshake must fail first", ex.Status, truncate([]byte(ex.Body), 200))
	}
	if ex.Err == "" {
		t.Fatal("invalid peer cert produced no TLS error and no status")
	}
	if strings.Contains(strings.ToLower(ex.Err), "401") {
		t.Fatalf("invalid cert was treated as a token failure: %s", ex.Err)
	}
	after := fingerprintRun(t, ctx, fe, member.HTTPBase(), job.ID, run.ID)
	requireNoMutation(t, before, after, "invalid mTLS peer")

	writeCoreRecord(t, fe, "invalid_mtls_peer", map[string]any{
		"run_id":       run.ID,
		"kind":         cli.Kind,
		"handshake":    "failed",
		"error_class":  "tls",
		"state_digest": digestOf(after),
	})
}
