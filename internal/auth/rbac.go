package auth

import (
	"maps"

	"github.com/caesium-cloud/caesium/internal/models"
)

// RouteClass says how the auth middleware authorises a route for a principal
// whose cluster role is below the route's role (a namespace-confined
// principal). A principal whose cluster role satisfies the route is authorised
// exactly as before namespaces existed, whatever the class.
type RouteClass string

const (
	// RouteClassCluster routes act on cluster-wide state: only the cluster
	// role counts.
	RouteClassCluster RouteClass = "cluster"
	// RouteClassNamespaced routes act on rows the middleware can resolve to
	// namespaces; the principal needs the role in every (or, where the
	// resolver says so, any) resolved namespace.
	RouteClassNamespaced RouteClass = "namespaced"
	// RouteClassCollection routes list rows across namespaces; the handler
	// filters them to the allowed set the middleware injects.
	RouteClassCollection RouteClass = "collection"
	// RouteClassIdentity routes describe the caller itself and are reachable
	// by any authenticated principal regardless of grants.
	RouteClassIdentity RouteClass = "identity"
)

// Valid reports whether c is one of the four route classes.
func (c RouteClass) Valid() bool {
	switch c {
	case RouteClassCluster, RouteClassNamespaced, RouteClassCollection, RouteClassIdentity:
		return true
	}
	return false
}

// RoutePolicy is the authorisation policy of one mounted route.
type RoutePolicy struct {
	// Role is the minimum role the route requires.
	Role models.Role
	// Class decides how a namespace-confined principal is authorised.
	Class RouteClass
	// PendingItem, when set, names the identity-and-access plan item whose
	// handler work must land before a namespace-confined principal may reach
	// the route: until then the handler can return rows owned by another
	// namespace, so the middleware treats the route as cluster for confined
	// principals (fail closed). Principals whose cluster role satisfies Role
	// are unaffected. The owning item clears the marker when it ships.
	PendingItem string
}

// Pending reports whether the route is still fail-closed for confined
// principals.
func (p RoutePolicy) Pending() bool {
	return p.PendingItem != ""
}

// LookupRoutePolicy returns the policy for a method and normalised route path.
func LookupRoutePolicy(method, path string) (RoutePolicy, bool) {
	policy, ok := endpointPolicy[method+" "+path]
	return policy, ok
}

// RoutePolicies returns a copy of the whole route table, keyed "METHOD /path".
// Tests use it to prove the table is complete and well formed.
func RoutePolicies() map[string]RoutePolicy {
	return maps.Clone(endpointPolicy)
}

// RequiredRole returns the minimum role needed for a given HTTP method + path.
func RequiredRole(method, path string) (models.Role, bool) {
	policy, ok := LookupRoutePolicy(method, path)
	return policy.Role, ok
}

// HasRole returns true if the key's role is at or above the required level.
func HasRole(keyRole, required models.Role) bool {
	return models.RoleLevel(keyRole) >= models.RoleLevel(required)
}

// CheckScope validates whether the key is allowed to act on the given job alias.
// A nil/empty scope means unrestricted access.
func CheckScope(scopeJSON []byte, jobAlias string) bool {
	scope, err := DecodeScope(scopeJSON)
	if err != nil {
		return false // malformed scope denies access
	}
	if scope == nil {
		return true
	}
	return containsScopedJob(scope.Jobs, jobAlias)
}

func clusterRoute(role models.Role) RoutePolicy {
	return RoutePolicy{Role: role, Class: RouteClassCluster}
}

func namespacedRoute(role models.Role) RoutePolicy {
	return RoutePolicy{Role: role, Class: RouteClassNamespaced}
}

func collectionRoute(role models.Role) RoutePolicy {
	return RoutePolicy{Role: role, Class: RouteClassCollection}
}

func identityRoute(role models.Role) RoutePolicy {
	return RoutePolicy{Role: role, Class: RouteClassIdentity}
}

// pendingOn marks a route fail-closed for confined principals until the named
// plan item lands (see RoutePolicy.PendingItem).
func (p RoutePolicy) pendingOn(item string) RoutePolicy {
	p.PendingItem = item
	return p
}

const (
	viewer   = models.RoleViewer
	runner   = models.RoleRunner
	operator = models.RoleOperator
	admin    = models.RoleAdmin
)

// endpointPolicy maps "METHOD /path-pattern" to the route's policy.
// Parametric segments are replaced with `:id` by the middleware normaliser.
//
// Class rules (docs/design/2026-09-16-identity-and-access-design.md §8.1):
// a route is namespaced only when the middleware can resolve the rows the
// handler acts on (api/middleware/auth_namespace_resolvers.go holds one
// resolver per namespaced route); a route whose response can include rows
// owned by another namespace (directly, via a shared trigger, via run history
// under a moved job, or via a global graph) is pendingOn the plan item that
// filters it.
var endpointPolicy = map[string]RoutePolicy{
	// Public — no auth required (handled by skip list, not here)

	// Identity: the caller itself, reachable with any (even empty) grants.
	"GET /auth/whoami":        identityRoute(viewer),
	"POST /auth/logout":       identityRoute(viewer),
	"GET /v1/system/features": identityRoute(viewer),

	// The Prometheus registry is global and its series carry job aliases and
	// task names, so a namespace-confined principal must not scrape it.
	"GET /metrics": clusterRoute(viewer),

	// Connector reads (execution-connectors W5). The handler answers only a
	// caller with a cluster viewer grant (connector.readPrincipal reads
	// ClusterRole), and a connection's executions span every namespace's runs.
	// Connector messages reuse the cluster role boundary. A Query is an explicit
	// read despite using POST; mutations always require operator or admin.
	"POST /v1/connectors/:id/executions/:id/queries/:id":  clusterRoute(viewer),
	"POST /v1/connectors/:id/executions/:id/actions/:id":  clusterRoute(operator),
	"GET /v1/connectors/:id/executions/:id/operations":    clusterRoute(viewer),
	"GET /v1/connectors/:id/operations/:id":               clusterRoute(viewer),
	"POST /v1/connectors/:id/operations/:id/resubmit":     clusterRoute(operator),
	"GET /v1/connectors":                                  clusterRoute(viewer),
	"GET /v1/connectors/:id/executions":                   clusterRoute(viewer),
	"GET /v1/connectors/:id/executions/:id":               clusterRoute(viewer),
	"GET /v1/connectors/:id/executions/:id/history":       clusterRoute(viewer),
	"GET /v1/connectors/:id/executions/:id/relationships": clusterRoute(viewer),

	// Jobs. The history reads under a job (its runs, latest run, run diff,
	// why baselines, cache entries and backfills) are authorised on the job's
	// current namespace and then filtered by each row's persisted namespace
	// (B4), so a moved job's old history stays with its old owner. Blame and
	// topology read the job's DAG snapshots, filtered by the namespace each
	// snapshot was written in; a legacy snapshot recording none is shown only
	// to unconfined callers (B7).
	"GET /v1/jobs":           collectionRoute(viewer),
	"GET /v1/jobs/:id":       namespacedRoute(viewer),
	"GET /v1/jobs/:id/tasks": namespacedRoute(viewer),
	"GET /v1/jobs/:id/dag":   namespacedRoute(viewer),
	// The manifest exports the job's trigger configuration (webhook secrets
	// included); for a confined viewer the handler re-checks the job and
	// withholds the configuration of a trigger that also serves a job it
	// cannot read, in one read snapshot (B9).
	"GET /v1/jobs/:id/manifest":         namespacedRoute(viewer),
	"GET /v1/jobs/:id/queue":            namespacedRoute(viewer),
	"GET /v1/jobs/:id/runs":             namespacedRoute(viewer),
	"GET /v1/jobs/:id/runs/diff":        namespacedRoute(viewer),
	"GET /v1/jobs/:id/blame":            namespacedRoute(viewer),
	"GET /v1/jobs/:id/topology":         namespacedRoute(viewer),
	"GET /v1/jobs/:id/topology/history": namespacedRoute(viewer),
	// Cache entries are filtered by the namespace of the run that filled them.
	"GET /v1/jobs/:id/cache":     namespacedRoute(viewer),
	"GET /v1/jobs/:id/backfills": namespacedRoute(viewer),

	// One run of a job: the job's current namespace and the run's persisted
	// namespace are both required.
	"GET /v1/jobs/:id/runs/:id":                      namespacedRoute(viewer),
	"GET /v1/jobs/:id/runs/:id/logs":                 namespacedRoute(viewer),
	"GET /v1/jobs/:id/runs/:id/receipt":              namespacedRoute(viewer),
	"POST /v1/jobs/:id/runs/:id/receipt/verify":      namespacedRoute(viewer),
	"GET /v1/jobs/:id/runs/:id/tasks/:id/descriptor": namespacedRoute(viewer),
	"GET /v1/jobs/:id/runs/:id/tasks/:id/partitions": namespacedRoute(viewer),
	// A run's external executions: the run is gated like the routes above, and
	// the handler lists links only for a cluster viewer (others get an empty list).
	"GET /v1/jobs/:id/runs/:id/external-executions": namespacedRoute(viewer),
	// The why explainer diffs against a baseline run (the cache origin or the
	// prior run of the task); a baseline in a namespace the caller cannot read
	// is never used.
	"GET /v1/jobs/:id/runs/:id/why": namespacedRoute(viewer),

	// A backfill's total_runs deduplicates dates against the job's runs in
	// the backfill's own persisted namespace only.
	"GET /v1/jobs/:id/backfills/:id": namespacedRoute(viewer),

	// Events and stats aggregate every job; they are filtered to the
	// caller's namespaces.
	"GET /v1/events":          collectionRoute(viewer),
	"GET /v1/events/ingested": collectionRoute(viewer),
	"GET /v1/stats":           collectionRoute(viewer),
	"GET /v1/stats/summary":   collectionRoute(viewer),

	// Cluster state.
	"GET /v1/system/nodes":      clusterRoute(viewer),
	"GET /v1/atoms":             clusterRoute(viewer),
	"GET /v1/atoms/:id":         clusterRoute(viewer),
	"GET /v1/nodes/:id/workers": clusterRoute(viewer),

	// Global cross-job graphs, computed whole and redacted at the boundary
	// for confined principals (B5, internal/authz/redact.go).
	"GET /v1/contracts/graph": collectionRoute(viewer),
	"GET /v1/lineage/impact":  collectionRoute(viewer),

	// Triggers resolve to the namespaces of every job they serve. A trigger's
	// event history shows a confined caller only deliveries whose every
	// started run sits in its namespaces.
	"GET /v1/triggers":            collectionRoute(viewer),
	"GET /v1/triggers/:id":        namespacedRoute(viewer),
	"GET /v1/triggers/:id/events": namespacedRoute(viewer),

	// Shared objects resolve to the object's own namespace ("*" = shared).
	// Their lists return the caller's namespaces plus "*" (D4).
	"GET /v1/notifications/channels":     collectionRoute(viewer),
	"GET /v1/notifications/channels/:id": namespacedRoute(viewer),
	"GET /v1/notifications/policies":     collectionRoute(viewer),
	"GET /v1/notifications/policies/:id": namespacedRoute(viewer),
	"GET /v1/agentprofiles":              collectionRoute(viewer),
	"GET /v1/agentprofiles/:id":          namespacedRoute(viewer),

	// Lint and diff resolve every definition's namespace and the stored
	// namespace of every alias they name; their handlers restrict the diff
	// baseline and the cross-job findings to the caller's namespaces and
	// redact foreign identities in validator errors (B2).
	"POST /v1/jobdefs/lint": namespacedRoute(viewer),
	"POST /v1/jobdefs/diff": namespacedRoute(viewer),

	// Incident operator read API (agent-in-the-loop D2). An incident belongs
	// to its failing run's namespace. Its frozen agent allowlist names only
	// jobs in that namespace, and for a confined reader the detail also drops
	// jobs that moved out and withholds what a pre-A8 agent session wrote
	// (A8).
	"GET /v1/incidents":     collectionRoute(viewer),
	"GET /v1/incidents/:id": namespacedRoute(viewer),

	// Dataset freshness operator surface (freshness-scheduling E1). A dataset
	// resolves to the namespaces of its declared producers: any of them for a
	// read, every one for a write. The lists hold only datasets with a
	// producer the caller may read, and every body is shaped by owner (B6:
	// derivations by their stamped namespace, metrics and a state's last run
	// by the run's persisted namespace; api/rest/service/dataset/tenancy.go).
	"GET /v1/datasets":                     collectionRoute(viewer),
	"GET /v1/datasets/holds":               collectionRoute(viewer),
	"GET /v1/datasets/:id/:id/metrics":     namespacedRoute(viewer),
	"GET /v1/datasets/:id/:id":             namespacedRoute(viewer),
	"GET /v1/datasets/:id/:id/derivations": namespacedRoute(viewer),
	// The manual watermark advance is a data-plane write at runner. A
	// confined caller's producer check is repeated inside the advance
	// transaction, and the returned state is shaped like a read (B6).
	"POST /v1/datasets/:id/:id/advance": namespacedRoute(runner),

	// Releasing a dataset hold (data-circuit-breaker C3) is an OPERATOR action,
	// the same tier as approving a tier-3 remediation: it overrides an automated
	// safety decision and lets downstream jobs run on data the breaker rejected.
	// A runner that can trigger runs must not also be able to switch the breaker
	// off. The route is only mounted when CAESIUM_DATA_ASSERTIONS_ENABLED is on;
	// this entry is unconditional, because an unlisted route is denied as
	// "unknown_route" (403) even to a valid admin key. A confined operator
	// needs the role in every producer namespace, re-checked inside the
	// release transaction; the returned hold drops its impact cone and any
	// run another namespace owns (B6).
	"POST /v1/datasets/holds/:id/release": namespacedRoute(operator),

	// Agent tool surface (/v1/agent/*). Reachable by an unscoped operator/admin
	// AND by an agent-session credential minted at the runner role; the
	// per-incident binding is enforced separately by the deny-by-default scope
	// switch (api/middleware/auth_scope.go), which 403s an agent token on any
	// route outside its own incident. Reads sit at viewer, mutating tool calls
	// (propose/execute an action, append a note) at runner. MCP multiplexes both
	// read and write tools over one POST, so it is agent/runner-only. Every
	// read is confined to the incident's namespace (A8): allow-listed jobs
	// that moved out are dropped, history and why baselines are the runs
	// recorded there, and a job that moved out shows neither its DAG nor its
	// remediation policy; actions refuse a target outside the namespace.
	"GET /v1/agent/incidents/:id/bundle":    namespacedRoute(viewer),
	"GET /v1/agent/incidents/:id/context/*": namespacedRoute(viewer),
	"POST /v1/agent/incidents/:id/actions":  namespacedRoute(runner),
	"POST /v1/agent/incidents/:id/notes":    namespacedRoute(runner),
	"POST /v1/agent/incidents/:id/mcp":      namespacedRoute(runner),

	// Runner
	"POST /v1/jobs/:id/run":                                     namespacedRoute(runner),
	"POST /v1/jobs/:id/runs/:id/replay":                         namespacedRoute(runner),
	"POST /v1/jobs/:id/runs/:id/retry":                          namespacedRoute(runner),
	"POST /v1/jobs/:id/runs/:id/tasks/:id/partitions/:id/retry": namespacedRoute(runner),
	"POST /v1/jobs/:id/runs/:id/callbacks/retry":                namespacedRoute(runner),
	// Backfill dates deduplicate against the backfill's own namespace (B4).
	"POST /v1/jobs/:id/backfill": namespacedRoute(runner),
	// Ingest routes an event to every matching job (A6 splits delivery by
	// namespace and reports skips).
	"POST /v1/events":            namespacedRoute(runner).pendingOn("A6"),
	"POST /v1/triggers/:id/fire": namespacedRoute(runner),

	// Operator
	// Create and apply resolve every definition's namespace and the stored
	// namespace of every existing alias; for a confined writer the handler
	// re-checks both, trigger tenancy (the rows and webhook paths a write
	// touches) and the prune set inside one transaction (B9).
	"POST /v1/jobs":                         namespacedRoute(operator),
	"DELETE /v1/jobs/:id":                   namespacedRoute(operator),
	"PUT /v1/jobs/:id/pause":                namespacedRoute(operator),
	"PUT /v1/jobs/:id/unpause":              namespacedRoute(operator),
	"POST /v1/jobdefs/apply":                namespacedRoute(operator),
	"POST /v1/cache/prune":                  clusterRoute(operator),
	"DELETE /v1/jobs/:id/queue/:id":         namespacedRoute(operator),
	"DELETE /v1/jobs/:id/cache":             namespacedRoute(operator),
	"DELETE /v1/jobs/:id/cache/:id":         namespacedRoute(operator),
	"PUT /v1/jobs/:id/backfills/:id/cancel": namespacedRoute(operator),
	// A new trigger serves no job yet, so it has no namespace to resolve.
	"POST /v1/triggers": clusterRoute(operator),
	// Editing a trigger needs the role in every job it serves; the handler
	// re-checks that and the webhook path it subscribes in its transaction
	// (B9 trigger tenancy).
	"PATCH /v1/triggers/:id": namespacedRoute(operator),
	"POST /v1/atoms":         clusterRoute(operator),
	"DELETE /v1/atoms/:id":   clusterRoute(operator),
	// Shared-object writes need operator in the object's namespace (the
	// create body's for POST), and at "*" for a cluster-shared object (D4).
	// A policy in a namespace references only that namespace's channels or
	// "*" ones and matches only events that namespace owns
	// (internal/notification RouteEvent), and an incident resolves only a
	// profile in its own namespace or "*" (internal/incident
	// ResolvePlaybook), so a confined write cannot route, silence or stand in
	// for another namespace's alerts or remediation.
	"POST /v1/notifications/channels":       namespacedRoute(operator),
	"PATCH /v1/notifications/channels/:id":  namespacedRoute(operator),
	"DELETE /v1/notifications/channels/:id": namespacedRoute(operator),
	"POST /v1/notifications/policies":       namespacedRoute(operator),
	"PATCH /v1/notifications/policies/:id":  namespacedRoute(operator),
	"DELETE /v1/notifications/policies/:id": namespacedRoute(operator),
	"POST /v1/agentprofiles":                namespacedRoute(operator),
	"PATCH /v1/agentprofiles/:id":           namespacedRoute(operator),
	"DELETE /v1/agentprofiles/:id":          namespacedRoute(operator),
	// Tier-3 approval decisions (agent-in-the-loop D1). Operator-gated; agent
	// session tokens are additionally rejected outright in authorizeScope.
	// Approving executes the proposed action against the incident's job (a
	// jobdef patch, a retry). The executor refuses a job that moved out of the
	// incident's namespace or a run recorded elsewhere, and confines a jobdef
	// patch to the namespace, triggers included (A8), so operator in the
	// incident's namespace covers everything it can touch. Rejecting only
	// records the decision on the incident.
	"POST /v1/incidents/:id/approvals/:id/approve": namespacedRoute(operator),
	"POST /v1/incidents/:id/approvals/:id/reject":  namespacedRoute(operator),

	// Admin
	"PUT /v1/logs/level":      clusterRoute(admin),
	"GET /v1/logs/level":      clusterRoute(admin),
	"GET /v1/logs/stream":     clusterRoute(admin),
	"GET /v1/database/schema": clusterRoute(admin),
	"POST /v1/database/query": clusterRoute(admin),
	// API keys (A5): a namespace admin lists, mints, revokes and rotates keys
	// confined to its namespaces; minting resolves the body's namespaces ("*"
	// when none), revoke and rotate the stored key's.
	"GET /v1/auth/keys":             collectionRoute(admin),
	"POST /v1/auth/keys":            namespacedRoute(admin),
	"POST /v1/auth/keys/:id/revoke": namespacedRoute(admin),
	"POST /v1/auth/keys/:id/rotate": namespacedRoute(admin),
	// The audit query authorises on ?namespace= (omitted means "*").
	"GET /v1/auth/audit": namespacedRoute(admin),
	// The access policy itself (A4).
	"GET /v1/auth/policy":         clusterRoute(admin),
	"POST /v1/auth/policy/reload": clusterRoute(admin),
	// Declared namespaces and their settings, never bindings (F3).
	"GET /v1/auth/namespaces": clusterRoute(viewer),
	// User and session administration (E3): cluster admins only.
	"GET /v1/auth/users":                      clusterRoute(admin),
	"GET /v1/auth/users/:id":                  clusterRoute(admin),
	"POST /v1/auth/users/:id/disable":         clusterRoute(admin),
	"POST /v1/auth/users/:id/enable":          clusterRoute(admin),
	"POST /v1/auth/users/:id/sessions/revoke": clusterRoute(admin),
	"GET /v1/auth/sessions":                   clusterRoute(admin),
	// Removes a stale dqlite member from the raft configuration (#582
	// follow-up). Unscoped admins only: the scope middleware denies any
	// job-scoped key on a route it has no job to check against.
	"DELETE /v1/system/nodes/:id": clusterRoute(admin),
}
