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

	// Jobs. GET /v1/jobs/:id attaches the latest run, and the history reads
	// below return runs by job id alone; under a moved job those rows can sit
	// in the old namespace (B4 filters them by the run's persisted namespace).
	"GET /v1/jobs":           collectionRoute(viewer).pendingOn("B4"),
	"GET /v1/jobs/:id":       namespacedRoute(viewer).pendingOn("B4"),
	"GET /v1/jobs/:id/tasks": namespacedRoute(viewer),
	"GET /v1/jobs/:id/dag":   namespacedRoute(viewer),
	// The manifest exports the job's trigger configuration (webhook secrets
	// included), and a trigger may also serve another namespace's job (B9 owns
	// shared-trigger ownership on the apply/export round trip).
	"GET /v1/jobs/:id/manifest":         namespacedRoute(viewer).pendingOn("B9"),
	"GET /v1/jobs/:id/queue":            namespacedRoute(viewer),
	"GET /v1/jobs/:id/runs":             namespacedRoute(viewer).pendingOn("B4"),
	"GET /v1/jobs/:id/runs/diff":        namespacedRoute(viewer).pendingOn("B4"),
	"GET /v1/jobs/:id/blame":            namespacedRoute(viewer).pendingOn("B4"),
	"GET /v1/jobs/:id/topology":         namespacedRoute(viewer).pendingOn("B4"),
	"GET /v1/jobs/:id/topology/history": namespacedRoute(viewer).pendingOn("B4"),
	// Cache entries carry the run ids and results of the runs that filled them.
	"GET /v1/jobs/:id/cache":     namespacedRoute(viewer).pendingOn("B4"),
	"GET /v1/jobs/:id/backfills": namespacedRoute(viewer).pendingOn("B4"),

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
	// prior run of the task), which can be another namespace's history.
	"GET /v1/jobs/:id/runs/:id/why": namespacedRoute(viewer).pendingOn("B4"),

	// A backfill's total_runs is computed when it starts by deduplicating
	// dates against the job's runs by job id alone
	// (internal/backfill/store.go LatestRunForLogicalDate), so under a moved
	// job it counts the old namespace's per-date runs and outcomes; reading
	// or cancelling it returns that row (B4 filters the dedupe by the run's
	// persisted namespace).
	"GET /v1/jobs/:id/backfills/:id": namespacedRoute(viewer).pendingOn("B4"),

	// Events and stats aggregate every job.
	"GET /v1/events":          collectionRoute(viewer).pendingOn("B4"),
	"GET /v1/events/ingested": collectionRoute(viewer).pendingOn("B4"),
	"GET /v1/stats":           collectionRoute(viewer).pendingOn("B4"),
	"GET /v1/stats/summary":   collectionRoute(viewer).pendingOn("B4"),

	// Cluster state.
	"GET /v1/system/nodes":      clusterRoute(viewer),
	"GET /v1/atoms":             clusterRoute(viewer),
	"GET /v1/atoms/:id":         clusterRoute(viewer),
	"GET /v1/nodes/:id/workers": clusterRoute(viewer),

	// Global cross-job graphs (B5 redacts them at the boundary).
	"GET /v1/contracts/graph": collectionRoute(viewer).pendingOn("B5"),
	"GET /v1/lineage/impact":  collectionRoute(viewer).pendingOn("B5"),

	// Triggers resolve to the namespaces of every job they serve. A trigger's
	// event history is keyed by trigger id alone.
	"GET /v1/triggers":            collectionRoute(viewer).pendingOn("B4"),
	"GET /v1/triggers/:id":        namespacedRoute(viewer),
	"GET /v1/triggers/:id/events": namespacedRoute(viewer).pendingOn("B4"),

	// Shared objects resolve to the object's own namespace ("*" = shared).
	"GET /v1/notifications/channels":     collectionRoute(viewer).pendingOn("D4"),
	"GET /v1/notifications/channels/:id": namespacedRoute(viewer),
	"GET /v1/notifications/policies":     collectionRoute(viewer).pendingOn("D4"),
	"GET /v1/notifications/policies/:id": namespacedRoute(viewer),
	"GET /v1/agentprofiles":              collectionRoute(viewer).pendingOn("D4"),
	"GET /v1/agentprofiles/:id":          namespacedRoute(viewer),

	// Lint and diff resolve every definition's namespace and the stored
	// namespace of every alias they name; their handlers restrict the diff
	// baseline and the cross-job findings to the caller's namespaces and
	// redact foreign identities in validator errors (B2).
	"POST /v1/jobdefs/lint": namespacedRoute(viewer),
	"POST /v1/jobdefs/diff": namespacedRoute(viewer),

	// Incident operator read API (agent-in-the-loop D2). The detail carries
	// the incident's frozen agent allowlist, which a lineage walk fills with
	// downstream job aliases from any namespace (A8 confines it).
	"GET /v1/incidents":     collectionRoute(viewer).pendingOn("B4"),
	"GET /v1/incidents/:id": namespacedRoute(viewer).pendingOn("A8"),

	// Dataset freshness operator surface (freshness-scheduling E1). A dataset
	// resolves to the namespaces of its declared producers: any of them for a
	// read, every one for a write. The reads stay pending until B6 filters
	// derivations and metrics by the producing run's namespace.
	"GET /v1/datasets":                     collectionRoute(viewer).pendingOn("B6"),
	"GET /v1/datasets/holds":               collectionRoute(viewer).pendingOn("B6"),
	"GET /v1/datasets/:id/:id/metrics":     namespacedRoute(viewer).pendingOn("B6"),
	"GET /v1/datasets/:id/:id":             namespacedRoute(viewer).pendingOn("B6"),
	"GET /v1/datasets/:id/:id/derivations": namespacedRoute(viewer).pendingOn("B6"),
	// The manual watermark advance is a data-plane write at runner. Its
	// response is the persisted dataset state, whose last run and consumed
	// watermarks can predate a producer's move from another namespace (B6
	// filters them by the producing run's namespace).
	"POST /v1/datasets/:id/:id/advance": namespacedRoute(runner).pendingOn("B6"),

	// Releasing a dataset hold (data-circuit-breaker C3) is an OPERATOR action,
	// the same tier as approving a tier-3 remediation: it overrides an automated
	// safety decision and lets downstream jobs run on data the breaker rejected.
	// A runner that can trigger runs must not also be able to switch the breaker
	// off. The route is only mounted when CAESIUM_DATA_ASSERTIONS_ENABLED is on;
	// this entry is unconditional, because an unlisted route is denied as
	// "unknown_route" (403) even to a valid admin key. The response is the
	// whole hold, whose impact snapshot names downstream consumers in any
	// namespace (B6 redacts it).
	"POST /v1/datasets/holds/:id/release": namespacedRoute(operator).pendingOn("B6"),

	// Agent tool surface (/v1/agent/*). Reachable by an unscoped operator/admin
	// AND by an agent-session credential minted at the runner role; the
	// per-incident binding is enforced separately by the deny-by-default scope
	// switch (api/middleware/auth_scope.go), which 403s an agent token on any
	// route outside its own incident. Reads sit at viewer, mutating tool calls
	// (propose/execute an action, append a note) at runner. MCP multiplexes both
	// read and write tools over one POST, so it is agent/runner-only. The
	// bundle, context and MCP handlers return the job's unfiltered run history
	// and current topology (A8 confines them).
	"GET /v1/agent/incidents/:id/bundle":    namespacedRoute(viewer).pendingOn("A8"),
	"GET /v1/agent/incidents/:id/context/*": namespacedRoute(viewer).pendingOn("A8"),
	"POST /v1/agent/incidents/:id/actions":  namespacedRoute(runner).pendingOn("A8"),
	"POST /v1/agent/incidents/:id/notes":    namespacedRoute(runner).pendingOn("A8"),
	"POST /v1/agent/incidents/:id/mcp":      namespacedRoute(runner).pendingOn("A8"),

	// Runner
	"POST /v1/jobs/:id/run":                                     namespacedRoute(runner),
	"POST /v1/jobs/:id/runs/:id/replay":                         namespacedRoute(runner),
	"POST /v1/jobs/:id/runs/:id/retry":                          namespacedRoute(runner),
	"POST /v1/jobs/:id/runs/:id/tasks/:id/partitions/:id/retry": namespacedRoute(runner),
	"POST /v1/jobs/:id/runs/:id/callbacks/retry":                namespacedRoute(runner),
	// Backfill creation deduplicates dates against the job's runs by job id
	// alone, so total_runs would report another namespace's history (B4).
	"POST /v1/jobs/:id/backfill": namespacedRoute(runner).pendingOn("B4"),
	// Ingest routes an event to every matching job (A6 splits delivery by
	// namespace and reports skips).
	"POST /v1/events":            namespacedRoute(runner).pendingOn("A6"),
	"POST /v1/triggers/:id/fire": namespacedRoute(runner),

	// Operator
	// Create and apply must resolve every definition's namespace, the stored
	// namespace of every existing alias, the triggers an apply updates in place
	// (and their webhook-path subscribers) and the prune set, atomically (B9).
	"POST /v1/jobs":                         namespacedRoute(operator).pendingOn("B9"),
	"DELETE /v1/jobs/:id":                   namespacedRoute(operator),
	"PUT /v1/jobs/:id/pause":                namespacedRoute(operator),
	"PUT /v1/jobs/:id/unpause":              namespacedRoute(operator),
	"POST /v1/jobdefs/apply":                namespacedRoute(operator).pendingOn("B9"),
	"POST /v1/cache/prune":                  clusterRoute(operator),
	"DELETE /v1/jobs/:id/queue/:id":         namespacedRoute(operator),
	"DELETE /v1/jobs/:id/cache":             namespacedRoute(operator),
	"DELETE /v1/jobs/:id/cache/:id":         namespacedRoute(operator),
	"PUT /v1/jobs/:id/backfills/:id/cancel": namespacedRoute(operator).pendingOn("B4"), // returns the backfill row (see GET)
	// A new trigger serves no job yet, so it has no namespace to resolve.
	"POST /v1/triggers": clusterRoute(operator),
	// Editing a trigger's configuration can subscribe it to another
	// namespace's webhook path (paths may repeat, and a delivery fans out to
	// every trigger on the path); B9 owns trigger subscription ownership.
	"PATCH /v1/triggers/:id": namespacedRoute(operator).pendingOn("B9"),
	"POST /v1/atoms":         clusterRoute(operator),
	"DELETE /v1/atoms/:id":   clusterRoute(operator),
	// Notification dispatch (internal/notification MatchPolicies) matches
	// policies by event type and filters only, not by namespace, so creating
	// or editing a channel or policy would route other namespaces' events to
	// it, and deleting one (a policy with no job filter, or its channel) could
	// silence another namespace's alerts and policy-driven remediation (D4
	// makes dispatch namespace-aware). Reads by id stay live.
	"POST /v1/notifications/channels":       namespacedRoute(operator).pendingOn("D4"),
	"PATCH /v1/notifications/channels/:id":  namespacedRoute(operator).pendingOn("D4"),
	"DELETE /v1/notifications/channels/:id": namespacedRoute(operator).pendingOn("D4"),
	"POST /v1/notifications/policies":       namespacedRoute(operator).pendingOn("D4"),
	"PATCH /v1/notifications/policies/:id":  namespacedRoute(operator).pendingOn("D4"),
	"DELETE /v1/notifications/policies/:id": namespacedRoute(operator).pendingOn("D4"),
	// A job references its agent profile by name, and runtime resolution
	// (internal/incident playbook resolution) looks the name up globally, so a
	// confined create or rename could stand in for another namespace's
	// profile, and a confined delete could remove the profile other
	// namespaces' incidents resolve, disabling their remediation (D4 makes
	// runtime resolution namespace-aware). Reads by id stay live.
	"POST /v1/agentprofiles":       namespacedRoute(operator).pendingOn("D4"),
	"PATCH /v1/agentprofiles/:id":  namespacedRoute(operator).pendingOn("D4"),
	"DELETE /v1/agentprofiles/:id": namespacedRoute(operator).pendingOn("D4"),
	// Tier-3 approval decisions (agent-in-the-loop D1). Operator-gated; agent
	// session tokens are additionally rejected outright in authorizeScope.
	// Approving executes the proposed action against the incident's job (a
	// jobdef patch, a retry), which may have moved to another namespace since
	// the incident opened and can touch a trigger shared with other
	// namespaces; A8 confines execution to its real targets. Rejecting only
	// records the decision on the incident.
	"POST /v1/incidents/:id/approvals/:id/approve": namespacedRoute(operator).pendingOn("A8"),
	"POST /v1/incidents/:id/approvals/:id/reject":  namespacedRoute(operator),

	// Admin
	"PUT /v1/logs/level":      clusterRoute(admin),
	"GET /v1/logs/level":      clusterRoute(admin),
	"GET /v1/logs/stream":     clusterRoute(admin),
	"GET /v1/database/schema": clusterRoute(admin),
	"POST /v1/database/query": clusterRoute(admin),
	// Key minting and key administration move to the key's namespaces in A5
	// (minting constraint, principal switch, namespace-admin key list).
	"GET /v1/auth/keys":             collectionRoute(admin).pendingOn("A5"),
	"POST /v1/auth/keys":            namespacedRoute(admin).pendingOn("A5"),
	"POST /v1/auth/keys/:id/revoke": namespacedRoute(admin).pendingOn("A5"),
	"POST /v1/auth/keys/:id/rotate": namespacedRoute(admin).pendingOn("A5"),
	// The audit query authorises on ?namespace= (omitted means "*").
	"GET /v1/auth/audit": namespacedRoute(admin),
	// The access policy itself (A4).
	"GET /v1/auth/policy":         clusterRoute(admin),
	"POST /v1/auth/policy/reload": clusterRoute(admin),
	// Removes a stale dqlite member from the raft configuration (#582
	// follow-up). Unscoped admins only: the scope middleware denies any
	// job-scoped key on a route it has no job to check against.
	"DELETE /v1/system/nodes/:id": clusterRoute(admin),
}
