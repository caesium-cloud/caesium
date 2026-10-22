package authz

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// Aggregate redaction (identity-and-access design §8.3, plan item B5).
//
// The cross-job aggregates (GET /v1/lineage/impact, GET /v1/contracts/graph)
// are computed over the whole graph, then redacted at the response boundary
// for a namespace-confined caller: a node the caller may not read collapses to
// {id: <opaque>, kind: "external"}, and an edge touching one keeps only its
// endpoints. This file is the only place a foreign node's identity is turned
// into a placeholder id; the services decide which nodes are foreign.

// ExternalKind is the wire kind of a node collapsed for a confined caller.
const ExternalKind = "external"

// externalIDPrefix starts every placeholder id, so it can never collide with
// a real node id ("job:…", "dataset:…").
const externalIDPrefix = "external:"

// Redactor collapses foreign nodes for one response. Its key is random per
// response, so a placeholder id is stable within the response (two edges to
// the same foreign node share it) and unlinkable across responses. A nil
// *Redactor is an unrestricted caller: there is nothing to redact.
type Redactor struct {
	ctx context.Context
	key [32]byte
}

// NewRedactor returns the redactor for a restricted request, or nil when ctx
// carries no namespace restriction (a cluster viewer, or auth disabled), in
// which case the caller must serve the aggregate unchanged.
func NewRedactor(ctx context.Context) *Redactor {
	if !Restricted(ctx) {
		return nil
	}
	r := &Redactor{ctx: ctx}
	// crypto/rand.Read never returns an error (it aborts the process when the
	// platform source fails), so a placeholder is never keyed by zeros.
	_, _ = rand.Read(r.key[:])
	return r
}

// Visible reports whether a row whose owning namespaces are namespaces may be
// shown: every one must be allowed. No namespaces at all (an unowned node)
// is never visible, so an unresolvable owner is foreign.
func (r *Redactor) Visible(namespaces ...string) bool {
	if r == nil {
		return true
	}
	if len(namespaces) == 0 {
		return false
	}
	for _, namespace := range namespaces {
		if !NamespaceAllowed(r.ctx, namespace) {
			return false
		}
	}
	return true
}

// ExternalID is the placeholder id of the foreign node (or collapsed edge)
// identified by identity: an HMAC under the response key, never the identity
// (or a plain hash of it) itself.
func (r *Redactor) ExternalID(identity string) string {
	mac := hmac.New(sha256.New, r.key[:])
	_, _ = mac.Write([]byte(identity))
	return externalIDPrefix + hex.EncodeToString(mac.Sum(nil))[:32]
}

// Done records that the handler redacted the aggregate (MarkFilteredContext).
func (r *Redactor) Done() {
	if r != nil {
		MarkFilteredContext(r.ctx)
	}
}
