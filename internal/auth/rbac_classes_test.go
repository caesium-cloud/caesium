package auth

import (
	"regexp"
	"sort"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
)

// masterEndpointRoles is the role table as it stood before route classes
// (master 3194fe68, the identity-and-access W3 base). It is the oracle for the
// no-policy guarantee: with no policy file every principal holds its role at
// "*", so every decision must be exactly HasRole(role, masterEndpointRoles[route])
// (api/middleware/auth_namespace_oracle_test.go drives that through the
// middleware). Never edit an entry to make a test pass.
var masterEndpointRoles = map[string]models.Role{
	"GET /metrics":                              models.RoleViewer,
	"GET /auth/whoami":                          models.RoleViewer,
	"POST /auth/logout":                         models.RoleViewer,
	"GET /v1/jobs":                              models.RoleViewer,
	"GET /v1/jobs/:id":                          models.RoleViewer,
	"GET /v1/jobs/:id/tasks":                    models.RoleViewer,
	"GET /v1/jobs/:id/dag":                      models.RoleViewer,
	"GET /v1/jobs/:id/manifest":                 models.RoleViewer,
	"GET /v1/jobs/:id/queue":                    models.RoleViewer,
	"GET /v1/jobs/:id/runs":                     models.RoleViewer,
	"GET /v1/jobs/:id/runs/diff":                models.RoleViewer,
	"GET /v1/jobs/:id/runs/:id":                 models.RoleViewer,
	"GET /v1/jobs/:id/runs/:id/logs":            models.RoleViewer,
	"GET /v1/jobs/:id/runs/:id/why":             models.RoleViewer,
	"GET /v1/jobs/:id/blame":                    models.RoleViewer,
	"GET /v1/jobs/:id/topology":                 models.RoleViewer,
	"GET /v1/jobs/:id/topology/history":         models.RoleViewer,
	"GET /v1/jobs/:id/runs/:id/receipt":         models.RoleViewer,
	"POST /v1/jobs/:id/runs/:id/receipt/verify": models.RoleViewer,
	"GET /v1/jobs/:id/cache":                    models.RoleViewer,
	"GET /v1/jobs/:id/backfills":                models.RoleViewer,
	"GET /v1/jobs/:id/backfills/:id":            models.RoleViewer,
	"GET /v1/events":                            models.RoleViewer,
	"GET /v1/events/ingested":                   models.RoleViewer,
	"GET /v1/stats":                             models.RoleViewer,
	"GET /v1/stats/summary":                     models.RoleViewer,
	"GET /v1/system/features":                   models.RoleViewer,
	"GET /v1/system/nodes":                      models.RoleViewer,
	"GET /v1/contracts/graph":                   models.RoleViewer,
	"GET /v1/triggers":                          models.RoleViewer,
	"GET /v1/triggers/:id":                      models.RoleViewer,
	"GET /v1/triggers/:id/events":               models.RoleViewer,
	"GET /v1/atoms":                             models.RoleViewer,
	"GET /v1/atoms/:id":                         models.RoleViewer,
	"GET /v1/nodes/:id/workers":                 models.RoleViewer,
	"GET /v1/notifications/channels":            models.RoleViewer,
	"GET /v1/notifications/channels/:id":        models.RoleViewer,
	"GET /v1/notifications/policies":            models.RoleViewer,
	"GET /v1/notifications/policies/:id":        models.RoleViewer,
	"GET /v1/agentprofiles":                     models.RoleViewer,
	"GET /v1/agentprofiles/:id":                 models.RoleViewer,
	"POST /v1/jobdefs/lint":                     models.RoleViewer,
	"POST /v1/jobdefs/diff":                     models.RoleViewer,
	"GET /v1/lineage/impact":                    models.RoleViewer,

	"GET /v1/jobs/:id/runs/:id/tasks/:id/descriptor": models.RoleViewer,
	"GET /v1/jobs/:id/runs/:id/tasks/:id/partitions": models.RoleViewer,
	"GET /v1/incidents":                     models.RoleViewer,
	"GET /v1/incidents/:id":                 models.RoleViewer,
	"GET /v1/datasets":                      models.RoleViewer,
	"GET /v1/datasets/holds":                models.RoleViewer,
	"GET /v1/datasets/:id/:id/metrics":      models.RoleViewer,
	"GET /v1/datasets/:id/:id":              models.RoleViewer,
	"GET /v1/datasets/:id/:id/derivations":  models.RoleViewer,
	"POST /v1/datasets/:id/:id/advance":     models.RoleRunner,
	"POST /v1/datasets/holds/:id/release":   models.RoleOperator,
	"GET /v1/agent/incidents/:id/bundle":    models.RoleViewer,
	"GET /v1/agent/incidents/:id/context/*": models.RoleViewer,
	"POST /v1/agent/incidents/:id/actions":  models.RoleRunner,
	"POST /v1/agent/incidents/:id/notes":    models.RoleRunner,
	"POST /v1/agent/incidents/:id/mcp":      models.RoleRunner,

	"POST /v1/jobs/:id/run":                                     models.RoleRunner,
	"POST /v1/jobs/:id/runs/:id/replay":                         models.RoleRunner,
	"POST /v1/jobs/:id/runs/:id/retry":                          models.RoleRunner,
	"POST /v1/jobs/:id/runs/:id/tasks/:id/partitions/:id/retry": models.RoleRunner,
	"POST /v1/jobs/:id/runs/:id/callbacks/retry":                models.RoleRunner,
	"POST /v1/jobs/:id/backfill":                                models.RoleRunner,
	"POST /v1/events":                                           models.RoleRunner,
	"POST /v1/triggers/:id/fire":                                models.RoleRunner,

	"POST /v1/jobs":                                models.RoleOperator,
	"DELETE /v1/jobs/:id":                          models.RoleOperator,
	"PUT /v1/jobs/:id/pause":                       models.RoleOperator,
	"PUT /v1/jobs/:id/unpause":                     models.RoleOperator,
	"POST /v1/jobdefs/apply":                       models.RoleOperator,
	"POST /v1/cache/prune":                         models.RoleOperator,
	"DELETE /v1/jobs/:id/queue/:id":                models.RoleOperator,
	"DELETE /v1/jobs/:id/cache":                    models.RoleOperator,
	"DELETE /v1/jobs/:id/cache/:id":                models.RoleOperator,
	"PUT /v1/jobs/:id/backfills/:id/cancel":        models.RoleOperator,
	"POST /v1/triggers":                            models.RoleOperator,
	"PATCH /v1/triggers/:id":                       models.RoleOperator,
	"POST /v1/atoms":                               models.RoleOperator,
	"DELETE /v1/atoms/:id":                         models.RoleOperator,
	"POST /v1/notifications/channels":              models.RoleOperator,
	"PATCH /v1/notifications/channels/:id":         models.RoleOperator,
	"DELETE /v1/notifications/channels/:id":        models.RoleOperator,
	"POST /v1/notifications/policies":              models.RoleOperator,
	"PATCH /v1/notifications/policies/:id":         models.RoleOperator,
	"DELETE /v1/notifications/policies/:id":        models.RoleOperator,
	"POST /v1/agentprofiles":                       models.RoleOperator,
	"PATCH /v1/agentprofiles/:id":                  models.RoleOperator,
	"DELETE /v1/agentprofiles/:id":                 models.RoleOperator,
	"POST /v1/incidents/:id/approvals/:id/approve": models.RoleOperator,
	"POST /v1/incidents/:id/approvals/:id/reject":  models.RoleOperator,

	"PUT /v1/logs/level":            models.RoleAdmin,
	"GET /v1/logs/level":            models.RoleAdmin,
	"GET /v1/logs/stream":           models.RoleAdmin,
	"GET /v1/database/schema":       models.RoleAdmin,
	"POST /v1/database/query":       models.RoleAdmin,
	"GET /v1/auth/keys":             models.RoleAdmin,
	"POST /v1/auth/keys":            models.RoleAdmin,
	"POST /v1/auth/keys/:id/revoke": models.RoleAdmin,
	"POST /v1/auth/keys/:id/rotate": models.RoleAdmin,
	"GET /v1/auth/audit":            models.RoleAdmin,
	"DELETE /v1/system/nodes/:id":   models.RoleAdmin,
}

// routesAddedAfterMaster are entries endpointPolicy carries that master did
// not: A4's policy routes, pre-added in their classified shape, and the
// connector reads master gained after the W3 base (execution-connectors W5,
// #644, all viewer there).
var routesAddedAfterMaster = map[string]RoutePolicy{
	"GET /v1/auth/policy":         {Role: models.RoleAdmin, Class: RouteClassCluster},
	"POST /v1/auth/policy/reload": {Role: models.RoleAdmin, Class: RouteClassCluster},
	"GET /v1/auth/namespaces":     {Role: models.RoleViewer, Class: RouteClassCluster},

	"POST /v1/connectors/:id/executions/:id/queries/:id":  {Role: models.RoleViewer, Class: RouteClassCluster},
	"POST /v1/connectors/:id/executions/:id/actions/:id":  {Role: models.RoleOperator, Class: RouteClassCluster},
	"GET /v1/connectors/:id/executions/:id/operations":    {Role: models.RoleViewer, Class: RouteClassCluster},
	"GET /v1/connectors/:id/operations/:id":               {Role: models.RoleViewer, Class: RouteClassCluster},
	"POST /v1/connectors/:id/operations/:id/resubmit":     {Role: models.RoleOperator, Class: RouteClassCluster},
	"GET /v1/connectors":                                  {Role: models.RoleViewer, Class: RouteClassCluster},
	"GET /v1/connectors/:id/executions":                   {Role: models.RoleViewer, Class: RouteClassCluster},
	"GET /v1/connectors/:id/executions/:id":               {Role: models.RoleViewer, Class: RouteClassCluster},
	"GET /v1/connectors/:id/executions/:id/history":       {Role: models.RoleViewer, Class: RouteClassCluster},
	"GET /v1/connectors/:id/executions/:id/relationships": {Role: models.RoleViewer, Class: RouteClassCluster},
	"GET /v1/jobs/:id/runs/:id/external-executions":       {Role: models.RoleViewer, Class: RouteClassNamespaced},

	// User and session administration (E3).
	"GET /v1/auth/users":                      {Role: models.RoleAdmin, Class: RouteClassCluster},
	"GET /v1/auth/users/:id":                  {Role: models.RoleAdmin, Class: RouteClassCluster},
	"POST /v1/auth/users/:id/disable":         {Role: models.RoleAdmin, Class: RouteClassCluster},
	"POST /v1/auth/users/:id/enable":          {Role: models.RoleAdmin, Class: RouteClassCluster},
	"POST /v1/auth/users/:id/sessions/revoke": {Role: models.RoleAdmin, Class: RouteClassCluster},
	"GET /v1/auth/sessions":                   {Role: models.RoleAdmin, Class: RouteClassCluster},
}

// TestRoutePolicyRolesMatchMaster pins every route's role to master's: route
// classes add a namespace dimension and never change a role.
func TestRoutePolicyRolesMatchMaster(t *testing.T) {
	policies := RoutePolicies()
	for key, role := range masterEndpointRoles {
		policy, ok := policies[key]
		require.True(t, ok, "master route %s is missing from endpointPolicy", key)
		require.Equal(t, role, policy.Role, "route %s changed role", key)
	}
	for key, policy := range policies {
		if _, ok := masterEndpointRoles[key]; ok {
			continue
		}
		added, ok := routesAddedAfterMaster[key]
		require.True(t, ok, "route %s is not on master; add it to routesAddedAfterMaster deliberately", key)
		require.Equal(t, added, policy, "route %s", key)
	}
}

var pendingItemPattern = regexp.MustCompile(`^[A-H][0-9]+$`)

// TestRoutePoliciesAreWellFormed checks every entry: a valid role and class,
// identity only for the caller's own routes, a plan item on every pending
// route, and no pending cluster or identity route (nothing to wait for).
func TestRoutePoliciesAreWellFormed(t *testing.T) {
	identity := map[string]bool{}
	for key, policy := range RoutePolicies() {
		require.True(t, models.ValidRole(string(policy.Role)), "route %s has invalid role %q", key, policy.Role)
		require.True(t, policy.Class.Valid(), "route %s has invalid class %q", key, policy.Class)
		if policy.Pending() {
			require.Regexp(t, pendingItemPattern, policy.PendingItem, "route %s pending item must name a plan item", key)
			require.Contains(t, []RouteClass{RouteClassNamespaced, RouteClassCollection}, policy.Class,
				"route %s: only namespaced and collection routes can be pending", key)
		}
		if policy.Class == RouteClassIdentity {
			identity[key] = true
		}
	}
	require.Equal(t, map[string]bool{
		"GET /auth/whoami":        true,
		"POST /auth/logout":       true,
		"GET /v1/system/features": true,
	}, identity, "identity routes are exactly the caller's own (design §8.1)")
	require.False(t, RouteClass("tenant").Valid())
	require.False(t, RouteClass("").Valid())
}

// TestRoutePolicyClassesPinned pins the class decisions the design and the
// wave's orchestrator decisions name, so a reclassification is deliberate.
func TestRoutePolicyClassesPinned(t *testing.T) {
	cases := map[string]RoutePolicy{
		"GET /metrics":                                 {Role: models.RoleViewer, Class: RouteClassCluster},
		"GET /v1/system/nodes":                         {Role: models.RoleViewer, Class: RouteClassCluster},
		"DELETE /v1/system/nodes/:id":                  {Role: models.RoleAdmin, Class: RouteClassCluster},
		"GET /v1/nodes/:id/workers":                    {Role: models.RoleViewer, Class: RouteClassCluster},
		"GET /v1/atoms":                                {Role: models.RoleViewer, Class: RouteClassCluster},
		"POST /v1/atoms":                               {Role: models.RoleOperator, Class: RouteClassCluster},
		"GET /v1/logs/stream":                          {Role: models.RoleAdmin, Class: RouteClassCluster},
		"POST /v1/database/query":                      {Role: models.RoleAdmin, Class: RouteClassCluster},
		"POST /v1/cache/prune":                         {Role: models.RoleOperator, Class: RouteClassCluster},
		"POST /v1/triggers":                            {Role: models.RoleOperator, Class: RouteClassCluster},
		"GET /v1/jobs":                                 {Role: models.RoleViewer, Class: RouteClassCollection},
		"GET /v1/auth/keys":                            {Role: models.RoleAdmin, Class: RouteClassCollection},
		"POST /v1/auth/keys":                           {Role: models.RoleAdmin, Class: RouteClassNamespaced},
		"POST /v1/auth/keys/:id/revoke":                {Role: models.RoleAdmin, Class: RouteClassNamespaced},
		"POST /v1/auth/keys/:id/rotate":                {Role: models.RoleAdmin, Class: RouteClassNamespaced},
		"GET /v1/jobs/:id/dag":                         {Role: models.RoleViewer, Class: RouteClassNamespaced},
		"GET /v1/jobs/:id/manifest":                    {Role: models.RoleViewer, Class: RouteClassNamespaced},
		"GET /v1/jobs/:id":                             {Role: models.RoleViewer, Class: RouteClassNamespaced},
		"POST /v1/jobs/:id/run":                        {Role: models.RoleRunner, Class: RouteClassNamespaced},
		"POST /v1/jobs/:id/runs/:id/replay":            {Role: models.RoleRunner, Class: RouteClassNamespaced},
		"POST /v1/jobdefs/apply":                       {Role: models.RoleOperator, Class: RouteClassNamespaced},
		"POST /v1/jobdefs/diff":                        {Role: models.RoleViewer, Class: RouteClassNamespaced},
		"POST /v1/jobdefs/lint":                        {Role: models.RoleViewer, Class: RouteClassNamespaced},
		"PATCH /v1/triggers/:id":                       {Role: models.RoleOperator, Class: RouteClassNamespaced},
		"POST /v1/jobs":                                {Role: models.RoleOperator, Class: RouteClassNamespaced},
		"POST /v1/events":                              {Role: models.RoleRunner, Class: RouteClassNamespaced, PendingItem: "A6"},
		"POST /v1/datasets/holds/:id/release":          {Role: models.RoleOperator, Class: RouteClassNamespaced, PendingItem: "B6"},
		"GET /v1/incidents/:id":                        {Role: models.RoleViewer, Class: RouteClassNamespaced, PendingItem: "A8"},
		"POST /v1/datasets/:id/:id/advance":            {Role: models.RoleRunner, Class: RouteClassNamespaced, PendingItem: "B6"},
		"GET /v1/datasets/:id/:id":                     {Role: models.RoleViewer, Class: RouteClassNamespaced, PendingItem: "B6"},
		"GET /v1/auth/audit":                           {Role: models.RoleAdmin, Class: RouteClassNamespaced},
		"POST /v1/agent/incidents/:id/mcp":             {Role: models.RoleRunner, Class: RouteClassNamespaced, PendingItem: "A8"},
		"POST /v1/incidents/:id/approvals/:id/approve": {Role: models.RoleOperator, Class: RouteClassNamespaced, PendingItem: "A8"},
		"POST /v1/incidents/:id/approvals/:id/reject":  {Role: models.RoleOperator, Class: RouteClassNamespaced},
		// Backfill date dedupe counts only the backfill's own namespace (B4).
		"POST /v1/jobs/:id/backfill":            {Role: models.RoleRunner, Class: RouteClassNamespaced},
		"GET /v1/jobs/:id/backfills/:id":        {Role: models.RoleViewer, Class: RouteClassNamespaced},
		"PUT /v1/jobs/:id/backfills/:id/cancel": {Role: models.RoleOperator, Class: RouteClassNamespaced},
		// DAG snapshots record no namespace (B7).
		"GET /v1/jobs/:id/blame":            {Role: models.RoleViewer, Class: RouteClassNamespaced, PendingItem: "B7"},
		"GET /v1/jobs/:id/topology":         {Role: models.RoleViewer, Class: RouteClassNamespaced, PendingItem: "B7"},
		"GET /v1/jobs/:id/topology/history": {Role: models.RoleViewer, Class: RouteClassNamespaced, PendingItem: "B7"},
		// Shared objects resolve to the object's namespace, and runtime
		// matching and profile resolution are namespace-aware (D4).
		"DELETE /v1/agentprofiles/:id":          {Role: models.RoleOperator, Class: RouteClassNamespaced},
		"DELETE /v1/notifications/channels/:id": {Role: models.RoleOperator, Class: RouteClassNamespaced},
		"DELETE /v1/notifications/policies/:id": {Role: models.RoleOperator, Class: RouteClassNamespaced},
		"POST /v1/agentprofiles":                {Role: models.RoleOperator, Class: RouteClassNamespaced},
		"POST /v1/notifications/policies":       {Role: models.RoleOperator, Class: RouteClassNamespaced},
		"GET /v1/notifications/channels":        {Role: models.RoleViewer, Class: RouteClassCollection},
		"GET /v1/agentprofiles/:id":             {Role: models.RoleViewer, Class: RouteClassNamespaced},
		"GET /v1/notifications/channels/:id":    {Role: models.RoleViewer, Class: RouteClassNamespaced},
		"GET /v1/notifications/policies/:id":    {Role: models.RoleViewer, Class: RouteClassNamespaced},
	}
	policies := RoutePolicies()
	for key, want := range cases {
		got, ok := policies[key]
		require.True(t, ok, key)
		require.Equal(t, want, got, key)
	}
}

// liveCollectionRoutes are the collection routes whose handlers filter by
// namespace (each is also listed, with the test that proves it, in
// api/auth_rbac_policy_completeness_test.go guardedCollectionRoutes).
var liveCollectionRoutes = map[string]bool{
	"GET /v1/auth/keys":       true, // A5
	"GET /v1/jobs":            true, // B4
	"GET /v1/triggers":        true,
	"GET /v1/incidents":       true,
	"GET /v1/stats":           true,
	"GET /v1/stats/summary":   true,
	"GET /v1/events":          true,
	"GET /v1/events/ingested": true,
	// D4: shared objects, the caller's namespaces plus "*".
	"GET /v1/notifications/channels": true,
	"GET /v1/notifications/policies": true,
	"GET /v1/agentprofiles":          true,
}

// TestPendingRoutesListed logs the pending set so a reviewer sees every route
// still fail-closed for confined principals, and pins that every collection
// route outside liveCollectionRoutes is pending (B5, B6, D4 filter them).
func TestPendingRoutesListed(t *testing.T) {
	var pending []string
	for key, policy := range RoutePolicies() {
		if policy.Pending() {
			pending = append(pending, policy.PendingItem+" "+key)
		}
		if policy.Class == RouteClassCollection {
			require.Equal(t, !liveCollectionRoutes[key], policy.Pending(), "collection route %s: pending unless listed in liveCollectionRoutes", key)
		}
	}
	for key := range liveCollectionRoutes {
		require.Equal(t, RouteClassCollection, RoutePolicies()[key].Class, "liveCollectionRoutes entry %s must be a collection route", key)
	}
	sort.Strings(pending)
	for _, line := range pending {
		t.Log(line)
	}
	require.NotEmpty(t, pending)
}

func TestLookupRoutePolicyUnknown(t *testing.T) {
	_, ok := LookupRoutePolicy("GET", "/v1/unknown")
	require.False(t, ok)
}
