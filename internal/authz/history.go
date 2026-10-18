package authz

import (
	"context"
	"slices"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
)

// History under a job (identity-and-access design §8.3, plan item B4).
//
// A route under /v1/jobs/:id is authorised on the job's CURRENT namespace, but
// the rows it reads (runs, backfills, cache entries, why baselines) keep the
// namespace they were written in: a moved job's old history stays with its
// old owner. The handler of such a read derives its query context with
// WithHistoryScope, and the store narrows its query with NamespaceFilter on
// the row's persisted namespace column.
//
// Only read handlers opt in. Write routes never carry the scope, because their
// background work inherits the request context (context.WithoutCancel keeps
// values) and must not be narrowed by the caller's grants.

// WithHistoryScope returns ctx restricted to the namespaces whose history p may
// read: those where p holds viewer. ctx is returned unchanged when p is nil
// (auth disabled), when p holds viewer at "*" (unrestricted, so the read is
// byte-identical to before), and when ctx already carries a restriction (a
// collection route's middleware scope, which the collection guard tracks by
// identity and must not be replaced).
func WithHistoryScope(ctx context.Context, p *auth.Principal) context.Context {
	if p == nil || scopeFrom(ctx) != nil {
		return ctx
	}
	allowed := p.AllowedNamespaces()
	if allowed == nil {
		return ctx
	}
	return WithAllowedNamespaces(ctx, allowed)
}

// NamespaceAllowed reports whether a row persisted in namespace may be
// returned under ctx's restriction. Unrestricted contexts allow everything; a
// legacy empty namespace counts as "default", exactly as NamespaceFilter
// treats it. Callers that filter in memory with it must call
// MarkFilteredContext.
func NamespaceAllowed(ctx context.Context, namespace string) bool {
	scope := scopeFrom(ctx)
	if scope == nil {
		return true
	}
	return slices.Contains(scope.allowed, models.NamespaceOrDefault(namespace))
}

// Restricted reports whether ctx carries a namespace restriction.
func Restricted(ctx context.Context) bool {
	return scopeFrom(ctx) != nil
}
