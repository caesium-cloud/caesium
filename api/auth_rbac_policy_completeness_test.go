package api

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"testing"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/api/rest/bind"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// mountEveryRoute mounts every route the API can serve: every feature gate
// that adds a route (bind.go and api.go conditionals) is switched on, so a
// gated route cannot escape the checks below.
func mountEveryRoute(t *testing.T) *echo.Echo {
	t.Helper()
	t.Cleanup(func() {
		require.NoError(t, env.Process())
	})

	t.Setenv("CAESIUM_AUTH_MODE", "api-key")
	t.Setenv("CAESIUM_CONNECTORS_ENABLED", "true")
	t.Setenv("CAESIUM_CONNECTORS_CONFIG_FILE", "/route-only-connectors.yaml")
	t.Setenv("CAESIUM_AUTH_OIDC_ENABLED", "true")
	t.Setenv("CAESIUM_AUTH_SAML_ENABLED", "true")
	t.Setenv("CAESIUM_AUTH_LDAP_ENABLED", "true")
	t.Setenv("CAESIUM_LOG_CONSOLE_ENABLED", "true")
	t.Setenv("CAESIUM_DATABASE_CONSOLE_ENABLED", "true")
	t.Setenv("CAESIUM_CONTRACT_ENFORCEMENT", "fail")
	t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "true")
	t.Setenv("CAESIUM_AGENT_REMEDIATION_ENABLED", "true")
	require.NoError(t, env.Process())

	vars := env.Variables()
	e := echo.New()
	authSvc := iauth.NewService(nil)
	sessions := iauth.NewSessionStore(nil)
	providers := SSOProviders{
		OIDC: routeOnlyRedirectProvider{name: "oidc"},
		SAML: routeOnlyRedirectProvider{name: "saml"},
		LDAP: routeOnlyCredentialProvider{name: "ldap"},
	}

	e.GET("/health", Health)
	e.GET("/auth/status", authStatus(vars))
	registerSSORoutes(e, vars, authSvc, nil, nil, sessions, nil, providers)
	registerMetrics(e, vars, authSvc, nil, nil, sessions)
	bind.All(e.Group("/v1"), nil, authSvc, nil, nil, sessions)
	return e
}

// mountedPolicyKeys returns every authenticated route as "METHOD /normalised".
func mountedPolicyKeys(e *echo.Echo) map[string]struct{} {
	mounted := make(map[string]struct{})
	for _, route := range e.Router().Routes() {
		// Echo 5.3 lists implicit group 404 handlers as echo_route_not_found.
		// Those are not API endpoints and do not belong in endpointPolicy.
		if route.Method == echo.RouteNotFound {
			continue
		}
		if authmw.IsPublicAuthPath(route.Path) {
			continue
		}
		mounted[route.Method+" "+authmw.NormalizeRoutePath(route.Path)] = struct{}{}
	}
	return mounted
}

func TestAuthMiddlewareMountedRoutesHaveRBACPolicy(t *testing.T) {
	mounted := mountedPolicyKeys(mountEveryRoute(t))

	// Spot-check that every gate really mounted its routes.
	for _, key := range []string{
		"GET /v1/connectors",
		"POST /v1/connectors/:id/executions/:id/queries/:id",
		"POST /v1/connectors/:id/executions/:id/actions/:id",
		"GET /v1/connectors/:id/executions/:id/operations",
		"GET /v1/connectors/:id/operations/:id",
		"POST /v1/connectors/:id/operations/:id/resubmit",
		"GET /v1/connectors/:id/executions/:id/relationships",
		"GET /v1/jobs/:id/runs/:id/external-executions",
		"GET /metrics",
		"GET /auth/whoami",
		"GET /v1/events",
		"GET /v1/auth/keys",
		"GET /v1/logs/level",
		"GET /v1/database/schema",
		"GET /v1/contracts/graph",
		"GET /v1/datasets/holds",
		"GET /v1/datasets/:id/:id/metrics",
		"POST /v1/datasets/holds/:id/release",
		"GET /v1/incidents",
		"GET /v1/incidents/:id",
		"POST /v1/incidents/:id/approvals/:id/approve",
		"POST /v1/incidents/:id/approvals/:id/reject",
		"GET /v1/auth/users",
		"POST /v1/auth/users/:id/sessions/revoke",
		"GET /v1/auth/sessions",
	} {
		require.Contains(t, mounted, key)
	}

	var missing []string
	for key := range mounted {
		if _, ok := iauth.RoutePolicies()[key]; !ok {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	require.Empty(t, missing, "auth-middleware mounted routes missing endpointPolicy entries")
}

var planItemPattern = regexp.MustCompile(`^[A-H][0-9]+$`)

// guardedCollectionRoutes are the live collection routes, each mapped to the
// test that proves its handler filters: the test serves the real handler
// behind the real auth middleware with AuthDeps.RequireCollectionFilter set,
// as a namespace-confined principal, and asserts the response holds only rows
// from the allowed namespaces. A collection route clears its pending marker
// only by being listed here with such a test.
var guardedCollectionRoutes = map[string]string{
	"GET /v1/auth/keys": "api/rest/controller/auth TestKeyRoutesCollectionGuard", // A5
	// identity-and-access B4.
	"GET /v1/jobs":            "api/rest/controller/job.TestJobListCollectionGuard",
	"GET /v1/triggers":        "api/rest/controller/trigger.TestTriggerListCollectionGuard",
	"GET /v1/incidents":       "api/rest/controller/incident.TestIncidentListCollectionGuard",
	"GET /v1/stats":           "api/rest/controller/stats.TestStatsCollectionGuard",
	"GET /v1/stats/summary":   "api/rest/controller/stats.TestStatsCollectionGuard",
	"GET /v1/events":          "api/rest/controller/event.TestEventStreamCollectionGuard",
	"GET /v1/events/ingested": "api/rest/controller/event.TestIngestedEventsCollectionGuard",
	// identity-and-access D4.
	"GET /v1/notifications/channels": "api/rest/controller/notification.TestChannelListCollectionGuard",
	"GET /v1/notifications/policies": "api/rest/controller/notification.TestPolicyListCollectionGuard",
	"GET /v1/agentprofiles":          "api/rest/controller/agentprofile.TestAgentProfileListCollectionGuard",
	// identity-and-access B5.
	"GET /v1/contracts/graph": "api/rest/controller/contract.TestContractGraphCollectionGuard",
	"GET /v1/lineage/impact":  "api/rest/controller/lineage.TestLineageImpactCollectionGuard",
}

// TestMountedRoutesAreClassified is the A3 build gate: every mounted route has
// a valid role and class; every live namespaced route has a namespace
// resolver (the middleware denies a confined principal on one that does not);
// every pending route names its owning plan item; and no collection route is
// live until its handler is driven through the collection guard.
func TestMountedRoutesAreClassified(t *testing.T) {
	mounted := mountedPolicyKeys(mountEveryRoute(t))
	policies := iauth.RoutePolicies()

	var problems, pending []string
	for key := range mounted {
		policy, ok := policies[key]
		if !ok {
			problems = append(problems, key+": unclassified")
			continue
		}
		if !models.ValidRole(string(policy.Role)) {
			problems = append(problems, key+": invalid role "+string(policy.Role))
		}
		if !policy.Class.Valid() {
			problems = append(problems, key+": invalid class "+string(policy.Class))
			continue
		}
		if policy.Pending() {
			if !planItemPattern.MatchString(policy.PendingItem) {
				problems = append(problems, key+": pending marker must name a plan item, got "+policy.PendingItem)
			}
			pending = append(pending, policy.PendingItem+"  "+key)
		}
		switch policy.Class {
		case iauth.RouteClassNamespaced:
			if !policy.Pending() && !authmw.HasNamespaceResolver(key) {
				problems = append(problems, key+": live namespaced route has no namespace resolver")
			}
		case iauth.RouteClassCollection:
			if !policy.Pending() && guardedCollectionRoutes[key] == "" {
				problems = append(problems, key+": live collection route; drive its handler through the collection guard (AuthDeps.RequireCollectionFilter) and list it in guardedCollectionRoutes before clearing its pending marker")
			}
		}
	}
	for key, test := range guardedCollectionRoutes {
		policy, ok := policies[key]
		_, isMounted := mounted[key]
		if !ok || !isMounted || policy.Class != iauth.RouteClassCollection || policy.Pending() || test == "" {
			problems = append(problems, key+": guardedCollectionRoutes entry must name a test for a mounted, live collection route")
		}
	}
	sort.Strings(problems)
	require.Empty(t, problems)

	sort.Strings(pending)
	for _, line := range pending {
		t.Log("pending", line)
	}
}

type routeOnlyRedirectProvider struct {
	name string
}

func (p routeOnlyRedirectProvider) Name() string {
	return p.name
}

func (p routeOnlyRedirectProvider) Begin(http.ResponseWriter, *http.Request, string) (string, error) {
	return "", nil
}

func (p routeOnlyRedirectProvider) Complete(*http.Request) (*iauth.ExternalIdentity, error) {
	return nil, nil
}

type routeOnlyCredentialProvider struct {
	name string
}

func (p routeOnlyCredentialProvider) Name() string {
	return p.name
}

func (p routeOnlyCredentialProvider) Authenticate(context.Context, string, string) (*iauth.ExternalIdentity, error) {
	return nil, nil
}
