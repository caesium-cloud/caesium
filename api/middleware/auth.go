package middleware

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/api/internal/auditlog"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/labstack/echo/v5"
)

// ContextKeyAuth is the key used to store the authenticated API key in the Echo context.
const ContextKeyAuth = "auth"

// ContextKeyPrincipal stores the unified authenticated identity for the request.
const ContextKeyPrincipal = "auth.principal"

// uuidPattern matches UUID path segments for route normalisation.
var uuidPattern = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
var namedParamPattern = regexp.MustCompile(`:[^/]+`)

// skipPaths lists exact paths that never require authentication.
var skipPaths = map[string]bool{
	"/health":       true,
	"/health/ready": true,
	"/health/live":  true,
}

var publicAuthPaths = map[string]bool{
	"/auth/status":            true,
	"/auth/sso/oidc/login":    true,
	"/auth/sso/oidc/callback": true,
	"/auth/sso/saml/login":    true,
	"/auth/sso/saml/acs":      true,
	"/auth/sso/saml/metadata": true,
	"/auth/sso/ldap/login":    true,
	// `caesium login` (identity-and-access E5): start stores a transaction and
	// exchange redeems a one-time code; both carry their own IP limiter.
	"/auth/cli/start":    true,
	"/auth/cli/exchange": true,
}

var publicAuthPathPrefixes = []string{
	"/v1/hooks/",
}

// AuthDeps bundles the dependencies the auth middleware needs.
type AuthDeps struct {
	Service    *auth.Service
	Auditor    *auth.AuditLogger
	Limiter    *auth.RateLimiter
	Sessions   *auth.SessionStore
	CookieName string

	// RequireCollectionFilter is test-only: when set, a confined principal's
	// successful request to a collection route whose handler did not call
	// authz.NamespaceFilter or authz.MarkFiltered fails with 500. Production
	// leaves it unset (the response is already written by then).
	RequireCollectionFilter bool
}

// Auth returns Echo middleware that enforces API-key or session-cookie
// authentication and RBAC.
func Auth(d AuthDeps) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			path := c.Request().URL.Path

			// Skip auth for explicitly public paths.
			if IsPublicAuthPath(path) {
				return next(c)
			}

			ip := c.RealIP()

			// Rate limit check before doing any work.
			if d.Limiter.IsLimited(ip) {
				metrics.AuthFailuresTotal.WithLabelValues("rate_limited").Inc()
				retryAfter := d.Limiter.RetryAfter(ip)
				c.Response().Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
				return echo.NewHTTPError(http.StatusTooManyRequests, "too many failed authentication attempts")
			}

			var key *models.APIKey
			var principal *auth.Principal

			token, malformedAuthorization := extractBearerToken(c)
			if malformedAuthorization {
				if recordCredentialFailure(c, d, ip, "authorization", "malformed_authorization") {
					return rateLimitError(c, d.Limiter, ip)
				}
				return echo.NewHTTPError(http.StatusUnauthorized, "invalid authorization header")
			}

			if token != "" {
				validKey, keyPrincipal, err := d.Service.AuthenticateKey(c.Request().Context(), token)
				if err != nil {
					reason := classifyAuthError(err)
					if recordCredentialFailure(c, d, ip, tokenPrefix(token), reason) {
						return rateLimitError(c, d.Limiter, ip)
					}
					if errors.Is(err, auth.ErrStaleGroups) {
						return staleGroupsError(c)
					}
					return echo.NewHTTPError(http.StatusUnauthorized, "invalid or expired api key")
				}

				principal = keyPrincipal
				// A user-bound key authenticates its user (E4) and is then
				// treated exactly like that user's session: no API-key
				// context, so GetAuthKey stays nil for it.
				if keyPrincipal.Kind == auth.PrincipalAPIKey {
					key = validKey
				}
			} else if d.Sessions != nil {
				if cookie, err := c.Request().Cookie(d.CookieName); err == nil && cookie.Value != "" {
					sess, user, verr := d.Sessions.Validate(c.Request().Context(), cookie.Value)
					if verr != nil {
						if recordCredentialFailure(c, d, ip, tokenPrefix(cookie.Value), classifySessionAuthError(verr)) {
							return rateLimitError(c, d.Limiter, ip)
						}
						if errors.Is(verr, auth.ErrStaleGroups) {
							return staleGroupsError(c)
						}
						return echo.NewHTTPError(http.StatusUnauthorized, "invalid or expired session")
					}
					if err := EnforceSessionCSRF(c, sess.CSRFToken); err != nil {
						if recordCredentialFailure(c, d, ip, tokenPrefix(cookie.Value), "csrf") {
							return rateLimitError(c, d.Limiter, ip)
						}
						return err
					}
					principal = auth.PrincipalFromUser(user, d.Service.PolicySource())
					c.Set(ContextKeyCSRFToken, sess.CSRFToken)
				}
			}

			if principal == nil {
				metrics.AuthFailuresTotal.WithLabelValues("missing").Inc()
				return echo.NewHTTPError(http.StatusUnauthorized, "missing credentials")
			}

			// Determine the route's policy.
			method := c.Request().Method
			routePath := normalisePath(c)
			policyKey := method + " " + routePath
			policy, ok := lookupRoutePolicy(method, routePath)
			if !ok {
				return denyAccess(c, d.Auditor, principal.Subject, principal.ClusterRole(), routePath, "unknown_route", "")
			}
			required := policy.Role

			// Namespace decision (A3): identity routes pass, a principal whose
			// cluster role satisfies the route passes exactly as before, and a
			// confined principal is authorised per route class.
			decision, err := authorizeRoute(c, d.Service, principal, policyKey, policy)
			if err != nil {
				if denial, ok := errors.AsType[*routeDenial](err); ok {
					resolveAuditNamespaces(c, d.Service, policyKey, decision)
					return denyDecision(c, d.Auditor, principal, routePath, decision, denial)
				}
				return err
			}

			// Store authenticated identity in context for downstream handlers.
			scopeContext, err := authorizeScope(c, d.Service, principal.Scope, routePath)
			if err != nil {
				if he, ok := err.(*echo.HTTPError); ok && he.Code == http.StatusForbidden {
					resolveAuditNamespaces(c, d.Service, policyKey, decision)
					return denyAccessAudited(c, d.Auditor, principal.Subject, principal.ClusterRole(), routePath, "insufficient_scope", required, forbiddenMessage(he), decision, nil)
				}
				return err
			}

			// An unrestricted principal's decision resolved no namespace; the
			// write routes that write an audit row resolve it best-effort for
			// the row. Read routes never pay for an audit lookup (their row,
			// if any, keeps an empty namespace).
			if decision.Unrestricted && method != http.MethodGet && routeWritesAudit(method, routePath) {
				resolveAuditNamespaces(c, d.Service, policyKey, decision)
			}

			if key != nil {
				c.Set(ContextKeyAuth, key)
				metrics.AuthKeyAgeSeconds.Observe(time.Since(key.CreatedAt).Seconds())
			}
			c.Set(ContextKeyPrincipal, principal)
			authz.SetDecision(c, decision)
			injectCollectionScope(c, decision)

			if err := next(c); err != nil {
				if denial, ok := errors.AsType[*authz.Denial](err); ok {
					return denyHandlerDenial(c, d.Auditor, principal, routePath, decision, denial)
				}
				return err
			}

			if d.RequireCollectionFilter {
				if err := checkCollectionFiltered(c, decision, routePath); err != nil {
					return err
				}
			}

			metrics.AuthRequestsTotal.WithLabelValues("success", string(principal.ClusterRole()), c.Request().Method, routePath).Inc()
			logSuccessfulAction(d.Auditor, c, principal, routePath, scopeContext, decision)
			return nil
		}
	}
}

// extractBearerToken extracts a Bearer token and reports malformed
// Authorization headers so cookie auth cannot silently mask them.
func extractBearerToken(c *echo.Context) (string, bool) {
	header := c.Request().Header.Get("Authorization")
	if header == "" {
		return "", false
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", true
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", true
	}
	return token, false
}

// IsPublicAuthPath reports whether path is intentionally reachable without the
// Auth middleware enforcing credentials. Keep RBAC completeness tests on this
// helper so their public-route exclusions match the middleware.
func IsPublicAuthPath(path string) bool {
	if skipPaths[path] || publicAuthPaths[path] {
		return true
	}
	for _, prefix := range publicAuthPathPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// NormalizeRoutePath returns the policy lookup path used by RBAC for Echo route
// patterns or raw request paths.
func NormalizeRoutePath(path string) string {
	path = uuidPattern.ReplaceAllString(path, ":id")
	return namedParamPattern.ReplaceAllString(path, ":id")
}

// normalisePath returns the Echo route pattern (with :param placeholders)
// so RBAC policy lookup works for parametric routes. Falls back to normalising
// UUID segments in the raw path.
func normalisePath(c *echo.Context) string {
	path := c.RouteInfo().Path
	if path == "" {
		path = c.Request().URL.Path
	}
	return NormalizeRoutePath(path)
}

// tokenPrefix returns a safe display prefix for logging, never the full key.
func tokenPrefix(token string) string {
	if len(token) > 13 {
		return token[:13]
	}
	return token
}

// classifyAuthError maps validation errors to metric label values.
func classifyAuthError(err error) string {
	switch err {
	case auth.ErrKeyNotFound:
		return "invalid"
	case auth.ErrKeyExpired:
		return "expired"
	case auth.ErrKeyRevoked:
		return "revoked"
	case auth.ErrUserDisabled:
		// A user-bound key whose user is disabled (E4). The client gets
		// the same 401 as for a revoked key; only the label differs.
		return "user_disabled"
	case auth.ErrStaleGroups:
		return staleGroupsReason
	default:
		log.Error("unexpected auth validation error", "error", err)
		return "error"
	}
}

func classifySessionAuthError(err error) string {
	switch err {
	case auth.ErrSessionInvalid:
		return "session_invalid"
	case auth.ErrSessionExpired:
		return "session_expired"
	case auth.ErrSessionRevoked:
		return "session_revoked"
	case auth.ErrUserDisabled:
		return "user_disabled"
	case auth.ErrStaleGroups:
		return staleGroupsReason
	default:
		log.Error("unexpected session validation error", "error", err)
		return "session_error"
	}
}

// staleGroupsReason is the 401 reason (and metric and audit label) for a
// user whose groups are past CAESIUM_AUTH_GROUP_MAX_STALENESS and could not
// be refreshed (identity-and-access §11.1): every credential of the user is
// refused until it signs in again.
const staleGroupsReason = "stale_groups"

// staleGroupsError answers a stale_groups refusal. Unlike the other 401s it
// says why, so the Console and the CLI can ask for a new sign-in.
func staleGroupsError(c *echo.Context) error {
	return c.JSON(http.StatusUnauthorized, map[string]string{
		"message": "group membership could not be confirmed; sign in again",
		"reason":  staleGroupsReason,
	})
}

func recordCredentialFailure(c *echo.Context, d AuthDeps, ip, actor, reason string) bool {
	metrics.AuthFailuresTotal.WithLabelValues(reason).Inc()
	limited := d.Limiter.RecordFailure(ip)
	auditlog.LogFailure(d.Auditor.Log(auth.AuditEntry{
		Actor:    actor,
		Action:   auth.ActionAuthDenied,
		SourceIP: ip,
		Outcome:  auth.OutcomeDenied,
		Metadata: map[string]any{
			"reason": reason,
			"method": c.Request().Method,
			"path":   c.Request().URL.Path,
		},
	}))
	return limited
}

func rateLimitError(c *echo.Context, limiter *auth.RateLimiter, ip string) error {
	retryAfter := limiter.RetryAfter(ip)
	c.Response().Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
	return echo.NewHTTPError(http.StatusTooManyRequests, "too many failed authentication attempts")
}

func denyAccess(
	c *echo.Context,
	auditor *auth.AuditLogger,
	actor string,
	keyRole models.Role,
	routePath string,
	reason string,
	required models.Role,
) error {
	return denyAccessWithMessage(c, auditor, actor, keyRole, routePath, reason, required, "insufficient permissions")
}

func denyAccessWithMessage(
	c *echo.Context,
	auditor *auth.AuditLogger,
	actor string,
	keyRole models.Role,
	routePath string,
	reason string,
	required models.Role,
	message string,
) error {
	return denyAccessAudited(c, auditor, actor, keyRole, routePath, reason, required, message, nil, nil)
}

// denyAccessAudited answers 403 with message, counts the denial and audits it.
// The row's namespace comes from the decision (nil: none). The fixed metadata
// fields keep their pre-namespace shape; extra fields are added to them.
func denyAccessAudited(
	c *echo.Context,
	auditor *auth.AuditLogger,
	actor string,
	keyRole models.Role,
	routePath string,
	reason string,
	required models.Role,
	message string,
	decision *authz.Decision,
	extra map[string]any,
) error {
	metrics.AuthRequestsTotal.WithLabelValues("denied", string(keyRole), c.Request().Method, routePath).Inc()

	metadata := map[string]any{
		"reason":     reason,
		"method":     c.Request().Method,
		"path":       routePath,
		"key_role":   string(keyRole),
		"policy_key": c.Request().Method + " " + routePath,
	}
	if required != "" {
		metadata["required_role"] = string(required)
	}
	auditDecisionMetadata(metadata, decision)
	for k, v := range extra {
		metadata[k] = v
	}

	auditlog.LogFailure(auditor.Log(auth.AuditEntry{
		Actor:     actor,
		Action:    auth.ActionAuthDenied,
		SourceIP:  c.RealIP(),
		Outcome:   auth.OutcomeDenied,
		Metadata:  metadata,
		Namespace: decision.AuditNamespace(),
	}))
	return echo.NewHTTPError(http.StatusForbidden, message)
}

// denyDecision audits a namespace-decision denial. A confined principal's row
// also names the namespaces it was checked against and the pending plan item;
// a principal holding nothing beyond its cluster role keeps the pre-namespace
// metadata.
func denyDecision(c *echo.Context, auditor *auth.AuditLogger, principal *auth.Principal, routePath string, decision *authz.Decision, denial *routeDenial) error {
	extra := map[string]any{}
	if confinedPrincipal(principal) {
		if decision != nil && len(decision.Namespaces) > 0 {
			extra["namespaces"] = append([]string(nil), decision.Namespaces...)
		}
		if denial.pending != "" {
			extra["pending"] = denial.pending
		}
	}
	return denyAccessAudited(c, auditor, principal.Subject, principal.ClusterRole(), routePath, denial.reason, denial.required, denial.message, decision, extra)
}

// denyHandlerDenial audits a refusal a handler returned as *authz.Denial (for
// example an idempotent start replaying a run the caller cannot read).
func denyHandlerDenial(c *echo.Context, auditor *auth.AuditLogger, principal *auth.Principal, routePath string, decision *authz.Decision, denial *authz.Denial) error {
	audited := &authz.Decision{Class: auth.RouteClassNamespaced, Role: denial.Required, Namespaces: denial.Namespaces}
	if decision != nil {
		audited.Class = decision.Class
	}
	reason := denial.Reason
	if reason == "" {
		reason = "insufficient_role"
	}
	message := denial.Message
	if message == "" {
		message = "insufficient permissions"
	}
	extra := map[string]any{"namespaces": append([]string(nil), denial.Namespaces...)}
	return denyAccessAudited(c, auditor, principal.Subject, principal.ClusterRole(), routePath, reason, denial.Required, message, audited, extra)
}

// confinedPrincipal reports whether p holds a grant outside "*" stronger than
// its cluster role: the only principals a namespace decision can treat
// differently from a cluster-role check.
func confinedPrincipal(p *auth.Principal) bool {
	if p == nil {
		return false
	}
	cluster := models.RoleLevel(p.ClusterRole())
	for namespace, role := range p.Grants {
		if namespace != models.ClusterNamespace && models.RoleLevel(role) > cluster {
			return true
		}
	}
	return false
}

func forbiddenMessage(err *echo.HTTPError) string {
	// echo/v5 HTTPError.Message is a concrete string (not any as in v4),
	// so read it directly rather than type-asserting.
	if err != nil && err.Message != "" {
		return err.Message
	}
	return "insufficient permissions"
}

func logSuccessfulAction(
	auditor *auth.AuditLogger,
	c *echo.Context,
	principal *auth.Principal,
	routePath string,
	scopeContext *scopeAuditContext,
	decision *authz.Decision,
) {
	action := auditActionForRoute(c.Request().Method, routePath)
	if action == "" {
		return
	}

	if resp, err := echo.UnwrapResponse(c.Response()); err == nil && resp.Status >= http.StatusBadRequest {
		return
	}

	entry := auth.AuditEntry{
		Actor:    principal.Subject,
		Action:   action,
		SourceIP: c.RealIP(),
		Outcome:  auth.OutcomeSuccess,
		Metadata: map[string]any{
			"method": c.Request().Method,
			"path":   routePath,
		},
		Namespace: decision.AuditNamespace(),
	}
	auditDecisionMetadata(entry.Metadata, decision)

	if scopeContext != nil && len(scopeContext.jobAliases) > 0 {
		entry.ResourceType = "job"
		entry.ResourceID = scopeContext.jobAliases[0]
		entry.Metadata["job_aliases"] = append([]string(nil), scopeContext.jobAliases...)
	}

	switch action {
	case auth.ActionJobdefApply:
		entry.ResourceType = "job_definition"
	case auth.ActionCachePrune:
		entry.ResourceType = "cache"
	case auth.ActionCacheDelete:
		entry.ResourceType = "cache"
	case auth.ActionClusterMemberRemove:
		entry.ResourceType = "cluster_member"
		entry.ResourceID = c.Param("id")
	}

	auditlog.LogFailure(auditor.Log(entry))
}

func auditActionForRoute(method, routePath string) string {
	switch method + " " + routePath {
	case "POST /v1/jobs":
		return auth.ActionJobCreate
	case "DELETE /v1/jobs/:id":
		return auth.ActionJobDelete
	case "PUT /v1/jobs/:id/pause":
		return auth.ActionJobPause
	case "PUT /v1/jobs/:id/unpause":
		return auth.ActionJobUnpause
	case "POST /v1/jobs/:id/run":
		return auth.ActionRunTrigger
	case "GET /v1/jobs/:id/queue":
		return auth.ActionRunQueueRead
	case "DELETE /v1/jobs/:id/queue/:id":
		return auth.ActionRunQueueCancel
	case "POST /v1/jobs/:id/runs/:id/retry":
		return auth.ActionRunRetry
	case "POST /v1/jobs/:id/backfill":
		return auth.ActionBackfill
	case "PUT /v1/jobs/:id/backfills/:id/cancel":
		return auth.ActionBackfill
	case "POST /v1/jobdefs/apply":
		return auth.ActionJobdefApply
	case "POST /v1/cache/prune":
		return auth.ActionCachePrune
	case "DELETE /v1/jobs/:id/cache":
		return auth.ActionCacheDelete
	case "DELETE /v1/jobs/:id/cache/:id":
		return auth.ActionCacheDelete
	case "DELETE /v1/system/nodes/:id":
		return auth.ActionClusterMemberRemove
	default:
		return ""
	}
}

// GetAuthKey extracts the authenticated API key from the Echo context.
// Returns nil if no key is present (e.g. unauthenticated endpoints).
func GetAuthKey(c *echo.Context) *models.APIKey {
	v := c.Get(ContextKeyAuth)
	if v == nil {
		return nil
	}
	key, ok := v.(*models.APIKey)
	if !ok {
		return nil
	}
	return key
}

// GetPrincipal returns the unified authenticated identity, or nil if unauthenticated.
func GetPrincipal(c *echo.Context) *auth.Principal {
	v := c.Get(ContextKeyPrincipal)
	if v == nil {
		return nil
	}
	principal, ok := v.(*auth.Principal)
	if !ok {
		return nil
	}
	return principal
}
