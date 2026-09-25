//go:build integration

package robustness

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/test/robustness/cluster"
)

func runWrongToken(t *testing.T, fe *faultEnv) {
	refreshTopo(t, fe)
	ctx := context.Background()
	member := fe.leader
	job, run, lease := applyBlockedRun(t, fe, member, "auth-wrong")
	before := fingerprintRun(t, ctx, fe, member.HTTPBase(), job.ID, run.ID)
	cli := wrongTokenClient(t, fe, member)
	owner := memberByNode(t, fe, lease.OwnerNode)
	target := cluster.InternalBase(owner.IP)

	completeEx := cli.Complete(ctx, target, completePayload(run, lease, "succeeded", lease.Generation))
	if completeEx.Err != "" {
		t.Fatalf("wrong-token complete transport: %v", completeEx.Err)
	}
	requireHTTPStatus(t, completeEx.Status, http.StatusUnauthorized, completeEx.Body)
	code, _ := ParseRefusal(completeEx.Status, []byte(completeEx.Body))
	if code != RefusalUnauthorized {
		t.Fatalf("wrong-token complete code %q, want unauthorized", code)
	}

	peer := otherMember(fe.topo, owner)
	dispatchEx := cli.Dispatch(ctx, cluster.InternalBase(peer.IP), dispatchPayload(run, lease, peer.NodeAddress))
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
		"target_complete": owner.Name,
		"target_dispatch": peer.Name,
		"state_digest":    digestOf(after),
	})
}

func runInvalidMTLS(t *testing.T, fe *faultEnv) {
	refreshTopo(t, fe)
	ctx := context.Background()
	member := fe.leader
	job, run, lease := applyBlockedRun(t, fe, member, "auth-mtls")
	before := fingerprintRun(t, ctx, fe, member.HTTPBase(), job.ID, run.ID)
	owner := memberByNode(t, fe, lease.OwnerNode)
	target := cluster.InternalBase(owner.IP)
	payload := completePayload(run, lease, "succeeded", lease.Generation)

	mintCtx, mintCancel := context.WithTimeout(ctx, 30*time.Second)
	cli, err := cluster.InvalidCertClient(mintCtx, fe.httpAPI, member.HTTPBase())
	mintCancel()
	if err != nil {
		t.Fatalf("invalid-cert client: %v", err)
	}
	ex := cli.Complete(ctx, target, payload)
	if ex.Err == "" && ex.Status != 0 {
		t.Fatalf("invalid peer cert reached the handler (status %d body %s); handshake must fail first", ex.Status, truncate([]byte(ex.Body), 200))
	}
	if !IsTLSHandshakeAlert(ex.Err) {
		t.Fatalf("invalid peer cert did not produce a TLS alert (err=%q status=%d)", ex.Err, ex.Status)
	}
	afterInvalid := fingerprintRun(t, ctx, fe, member.HTTPBase(), job.ID, run.ID)
	requireNoMutation(t, before, afterInvalid, "invalid mTLS peer")

	control := validInternalClient(t, fe, owner)
	ctrl := control.Complete(ctx, target, payload)
	if ctrl.Err != "" || ctrl.Status != http.StatusOK {
		t.Fatalf("valid-leaf control was not accepted on %s: err=%q status=%d body=%s",
			owner.Name, ctrl.Err, ctrl.Status, truncate([]byte(ctrl.Body), 200))
	}

	writeCoreRecord(t, fe, "invalid_mtls_peer", map[string]any{
		"run_id":          run.ID,
		"kind":            cli.Kind,
		"handshake":       "failed",
		"error_class":     "tls",
		"control_status":  ctrl.Status,
		"control_reached": true,
		"state_digest":    digestOf(afterInvalid),
	})
}
