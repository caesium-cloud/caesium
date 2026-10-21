package middleware

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/labstack/echo/v5"
)

// Namespace-aware route authorisation (identity-and-access design §8.2,
// plan item A3).
//
//  1. identity routes pass for any authenticated principal.
//  2. A principal whose cluster role satisfies the route is authorised
//     exactly as before namespaces existed: no namespace lookup, no new
//     status code, no new denial. With no policy file every principal holds
//     its role at "*", so this is the whole decision.
//  3. Otherwise the principal is confined. If none of its grants reaches the
//     route's role it is denied insufficient_role before anything is parsed
//     or looked up (a missing target must stay a 403, not become a 404).
//  4. A pending route, and a cluster route, deny a confined principal.
//  5. A collection route injects the namespaces where the principal holds the
//     route's role (none ⇒ deny); a namespaced route resolves its target's
//     namespaces and requires the role in every (or any) of them.

// lookupRoutePolicy is the route table lookup; tests swap it to mount a
// fake live collection route for the collection-guard test.
var lookupRoutePolicy = auth.LookupRoutePolicy

// routeDenial is an RBAC refusal from the namespace decision. The middleware
// audits it and answers 403.
type routeDenial struct {
	reason   string
	required models.Role
	pending  string
	message  string
}

func (d *routeDenial) Error() string { return "auth: " + d.reason }

func denyInsufficientRole(required models.Role) *routeDenial {
	return &routeDenial{reason: "insufficient_role", required: required, message: "insufficient permissions"}
}

// authorizeRoute makes the namespace part of the decision. It returns the
// decision and either nil (authorised), a *routeDenial (audited 403), or an
// HTTP error from target resolution (400/404/409/413/415/500), which is
// returned to the client as the handler would have returned it.
func authorizeRoute(c *echo.Context, svc *auth.Service, p *auth.Principal, policyKey string, rp auth.RoutePolicy) (*authz.Decision, error) {
	decision := &authz.Decision{Class: rp.Class, Role: rp.Role, PendingItem: rp.PendingItem}

	if rp.Class == auth.RouteClassIdentity {
		return decision, nil
	}
	if auth.HasRole(p.ClusterRole(), rp.Role) {
		decision.Unrestricted = true
		return decision, nil
	}
	if !auth.HasRole(highestRole(p), rp.Role) {
		return decision, denyInsufficientRole(rp.Role)
	}
	if rp.Pending() {
		denial := denyInsufficientRole(rp.Role)
		denial.pending = rp.PendingItem
		return decision, denial
	}

	switch rp.Class {
	case auth.RouteClassCollection:
		allowed := NamespacesGranting(p, rp.Role)
		if len(allowed) == 0 {
			return decision, denyInsufficientRole(rp.Role)
		}
		decision.Namespaces = allowed
		return decision, nil
	case auth.RouteClassNamespaced:
		resolver, ok := namespaceResolvers[policyKey]
		if !ok {
			// A build-time test failure (api/auth_rbac_policy_completeness_test.go);
			// at runtime the route is cluster-only.
			log.Error("namespaced route has no namespace resolver; denying confined principal", "route", policyKey)
			return decision, denyInsufficientRole(rp.Role)
		}
		// Read the whole request body before resolving, so the decision is
		// made after the client has sent everything: otherwise a client could
		// hold a request open past authorisation and complete it after the
		// target moved to another namespace.
		if err := bufferConfinedBody(c); err != nil {
			return decision, err
		}
		requirements, err := resolver(c, svc, rp.Role)
		if err != nil {
			return decision, err
		}
		decision.Namespaces = requirementNamespaces(requirements)
		if !requirementsSatisfied(p, requirements) {
			denial := denyInsufficientRole(rp.Role)
			if missing := disclosedMissingNamespaces(p, requirements); len(missing) > 0 {
				noun := "namespace"
				if len(missing) > 1 {
					noun = "namespaces"
				}
				denial.message = fmt.Sprintf("insufficient permissions: %s required in %s %s", rp.Role, noun, quotedList(missing))
			}
			return decision, denial
		}
		return decision, nil
	default:
		// cluster, or an unknown class: only the cluster role counts.
		return decision, denyInsufficientRole(rp.Role)
	}
}

// highestRole is the principal's strongest role across all of its grants.
func highestRole(p *auth.Principal) models.Role {
	var best models.Role
	if p == nil {
		return best
	}
	for _, role := range p.Grants {
		if models.RoleLevel(role) > models.RoleLevel(best) {
			best = role
		}
	}
	return best
}

// NamespacesGranting returns the sorted namespaces (never "*") in which p
// holds at least role.
func NamespacesGranting(p *auth.Principal, role models.Role) []string {
	allowed := make([]string, 0, len(p.Grants))
	for namespace := range p.Grants {
		if namespace == "" || namespace == models.ClusterNamespace {
			continue
		}
		if auth.HasRole(p.RoleIn(namespace), role) {
			allowed = append(allowed, namespace)
		}
	}
	slices.Sort(allowed)
	return allowed
}

// WriteConfinement reports how a write by p that needs role is confined
// (identity-and-access B9): namespaces are where p holds role (nil when it
// holds role cluster-wide) and aliases are its key's job-alias scope (nil
// when the key is unscoped). Either one confines the write. A nil p (auth
// disabled) is unconfined; an undecodable scope is an error, which callers
// treat as a denial.
func WriteConfinement(p *auth.Principal, role models.Role) (namespaces, aliases []string, err error) {
	if p == nil {
		return nil, nil, nil
	}
	if !auth.HasRole(p.ClusterRole(), role) {
		namespaces = NamespacesGranting(p, role)
	}
	jobs, err := auth.ScopeJobs(p.Scope)
	if err != nil {
		return nil, nil, err
	}
	if len(jobs) > 0 {
		aliases = jobs
	}
	return namespaces, aliases, nil
}

// ReadConfinement is WriteConfinement for reads: the namespaces p may read
// (Principal.AllowedNamespaces, nil when unrestricted) and its key's job-alias
// scope.
func ReadConfinement(p *auth.Principal) (namespaces, aliases []string, err error) {
	if p == nil {
		return nil, nil, nil
	}
	jobs, err := auth.ScopeJobs(p.Scope)
	if err != nil {
		return nil, nil, err
	}
	if len(jobs) > 0 {
		aliases = jobs
	}
	return p.AllowedNamespaces(), aliases, nil
}

// requirementsSatisfied evaluates every requirement; all must hold. Resolved
// namespaces are already normalised (never ""), but an empty one is mapped to
// "default" here again because Principal.RoleIn("") is the cluster role.
func requirementsSatisfied(p *auth.Principal, requirements []nsRequirement) bool {
	if len(requirements) == 0 {
		return false
	}
	for _, requirement := range requirements {
		if requirement.AnyGrant {
			if !auth.HasRole(highestRole(p), requirement.Role) {
				return false
			}
			continue
		}
		if len(requirement.Namespaces) == 0 {
			return false
		}
		matched := 0
		for _, namespace := range requirement.Namespaces {
			if auth.HasRole(p.RoleIn(models.NamespaceOrDefault(namespace)), requirement.Role) {
				matched++
			}
		}
		if requirement.Any && matched == 0 {
			return false
		}
		if !requirement.Any && matched != len(requirement.Namespaces) {
			return false
		}
	}
	return true
}

// disclosedMissingNamespaces returns, sorted and de-duplicated, the
// namespaces of the Disclose requirements in which p lacks the role: the
// namespaces a 403 may name because the caller's own request named them.
func disclosedMissingNamespaces(p *auth.Principal, requirements []nsRequirement) []string {
	var missing []string
	for _, requirement := range requirements {
		if !requirement.Disclose || requirement.Any || requirement.AnyGrant {
			continue
		}
		for _, namespace := range requirement.Namespaces {
			namespace = models.NamespaceOrDefault(namespace)
			if !auth.HasRole(p.RoleIn(namespace), requirement.Role) {
				missing = append(missing, namespace)
			}
		}
	}
	slices.Sort(missing)
	return slices.Compact(missing)
}

// quotedList renders namespaces as `"a", "b"`.
func quotedList(namespaces []string) string {
	quoted := make([]string, len(namespaces))
	for i, namespace := range namespaces {
		quoted[i] = strconv.Quote(namespace)
	}
	return strings.Join(quoted, ", ")
}

// requirementNamespaces is the sorted, de-duplicated union of the namespaces
// the requirements name, or nil when they name none.
func requirementNamespaces(requirements []nsRequirement) []string {
	var namespaces []string
	for _, requirement := range requirements {
		for _, namespace := range requirement.Namespaces {
			namespaces = append(namespaces, models.NamespaceOrDefault(namespace))
		}
	}
	if len(namespaces) == 0 {
		return nil
	}
	slices.Sort(namespaces)
	return slices.Compact(namespaces)
}

// controllerAuditedRoutes write their audit row outside the middleware but
// take its namespace from the decision.
var controllerAuditedRoutes = map[string]bool{
	"POST /v1/datasets/holds/:id/release": true,
}

func routeWritesAudit(method, routePath string) bool {
	return auditActionForRoute(method, routePath) != "" || controllerAuditedRoutes[method+" "+routePath]
}

// resolveAuditNamespaces fills decision.Namespaces for the audit row when the
// decision did not resolve them (an unrestricted principal, or a denial
// before resolution). Best-effort: any failure leaves them unresolved, and
// nothing here can change the response.
func resolveAuditNamespaces(c *echo.Context, svc *auth.Service, policyKey string, decision *authz.Decision) {
	if decision == nil || decision.Class != auth.RouteClassNamespaced || decision.Namespaces != nil {
		return
	}
	resolver, ok := namespaceResolvers[policyKey]
	if !ok {
		return
	}
	requirements, err := resolver(c, svc, decision.Role)
	if err != nil {
		return
	}
	decision.Namespaces = requirementNamespaces(requirements)
}

// auditDecisionMetadata adds the decision's namespaces to audit metadata when
// the row's namespace column cannot name them ("*" for several).
func auditDecisionMetadata(metadata map[string]any, decision *authz.Decision) {
	if decision != nil && decision.Class == auth.RouteClassNamespaced && len(decision.Namespaces) > 1 {
		metadata["namespaces"] = append([]string(nil), decision.Namespaces...)
	}
}

// injectCollectionScope stores a confined principal's allowed namespaces for a
// collection handler. Unrestricted principals get nothing, so their handlers
// see authz.AllowedNamespaces return restricted=false.
func injectCollectionScope(c *echo.Context, decision *authz.Decision) {
	if decision == nil || decision.Class != auth.RouteClassCollection || decision.Unrestricted {
		return
	}
	allowed := append([]string{}, decision.Namespaces...)
	c.Set(authz.ContextKeyAllowedNamespaces, allowed)
	req := c.Request()
	c.SetRequest(req.WithContext(authz.WithAllowedNamespaces(req.Context(), allowed)))
}

// errCollectionUnfiltered is the test-only collection guard's failure.
var errCollectionUnfiltered = echo.NewHTTPError(http.StatusInternalServerError, "collection handler did not apply the namespace filter")

// checkCollectionFiltered is the test-only post-handler guard (design §8.1):
// a confined principal's successful collection response must have been
// filtered through authz.NamespaceFilter or authz.MarkFiltered.
func checkCollectionFiltered(c *echo.Context, decision *authz.Decision, routePath string) error {
	if decision == nil || decision.Class != auth.RouteClassCollection || decision.Unrestricted {
		return nil
	}
	if resp, err := echo.UnwrapResponse(c.Response()); err == nil && resp.Status >= http.StatusBadRequest {
		return nil
	}
	if authz.Filtered(c.Request().Context()) {
		return nil
	}
	log.Error("collection handler returned rows without the namespace filter", "route", c.Request().Method+" "+routePath)
	return errCollectionUnfiltered
}
