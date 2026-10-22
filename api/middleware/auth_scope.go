package middleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// ContextKeyAllowedJobAliases stores the scoped job aliases available to list endpoints.
const ContextKeyAllowedJobAliases = "auth.allowed_job_aliases"

// LineageImpactScopedDenyMessage is the 403 reason returned to a scoped principal
// on the global, cross-job /v1/lineage/impact route. Exported so the auth and
// integration tests assert against the single source of truth.
const LineageImpactScopedDenyMessage = "lineage impact is a global cross-job query and requires an unscoped principal"

// ContractsGraphScopedDenyMessage is the 403 reason returned to a scoped
// principal on the global, cross-job /v1/contracts/graph route.
const ContractsGraphScopedDenyMessage = "contracts graph is a global cross-job query and requires an unscoped principal"

// EventsScopedDenyMessage is returned when a scoped principal attempts to open
// the global event stream without a run_id filter. Scoped event subscriptions
// must resolve to one run owner before any events are streamed.
const EventsScopedDenyMessage = "event stream requires an in-scope run_id for scoped principals"

// ApprovalAgentTokenDenyMessage is the 403 reason returned when a per-session
// agent token (Stream C1) attempts to approve or reject a tier-3 remediation
// action. An agent may NEVER approve its own proposed action — tier 3 always
// terminates at a human. Exported so the auth and integration tests assert
// against the single source of truth.
const ApprovalAgentTokenDenyMessage = "agent session tokens may not approve or reject remediation actions"

// AgentRoutePrefix is the only route prefix an agent-session credential may
// reach. Everything outside it is denied outright for agent tokens.
const AgentRoutePrefix = "/v1/agent/"

// AgentScopeDenyMessage is the 403 reason returned when an agent-session token
// is used on any route outside its incident's /v1/agent/* tool surface (a
// non-agent route, or a different incident's agent route). Exported so the auth
// and integration tests assert against the single source of truth.
const AgentScopeDenyMessage = "agent session token is scoped to its own incident's /v1/agent/* tool surface"

// isIncidentApprovalRoute reports whether routePath is one of the tier-3 approval
// decision routes. The path is already normalised (parametric segments collapsed
// to :id), so both approve and reject share this prefix:
//
//	POST /v1/incidents/:id/approvals/:id/approve
//	POST /v1/incidents/:id/approvals/:id/reject
func isIncidentApprovalRoute(routePath string) bool {
	return strings.HasPrefix(routePath, "/v1/incidents/:id/approvals/")
}

type scopeAuditContext struct {
	jobAliases []string
}

// GetAllowedJobAliases returns the scoped aliases injected by the auth middleware.
func GetAllowedJobAliases(c *echo.Context) []string {
	v := c.Get(ContextKeyAllowedJobAliases)
	if v == nil {
		return nil
	}
	aliases, ok := v.([]string)
	if !ok {
		return nil
	}
	return append([]string(nil), aliases...)
}

func authorizeScope(c *echo.Context, svc *auth.Service, scopeJSON []byte, routePath string) (*scopeAuditContext, error) {
	// Agent-session credentials are intercepted FIRST and are fully
	// self-contained: an agent token is valid only for its own incident's
	// /v1/agent/* routes and nothing else (Stream C1). This check must precede the
	// job-scope path because an agent key carries an empty job list — treating it
	// as "unrestricted" (the len==0 branch below) would be a privilege escalation,
	// letting an agent token reach every unscoped route.
	// Connector confinement also rejects malformed/zero-incident agent
	// markers. DecodeAgentClaim intentionally treats those as no valid claim;
	// they must not become unscoped connector credentials as a consequence.
	if strings.HasPrefix(routePath, "/v1/connectors") && len(scopeJSON) > 0 {
		var scope struct {
			Agent json.RawMessage `json:"agent"`
		}
		if json.Unmarshal(scopeJSON, &scope) != nil || (len(scope.Agent) > 0 && string(scope.Agent) != "null") {
			return nil, echo.NewHTTPError(http.StatusForbidden, AgentScopeDenyMessage)
		}
	}
	agentClaim, err := auth.DecodeAgentClaim(scopeJSON)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusForbidden, "insufficient permissions")
	}
	if agentClaim != nil {
		// D1 security precondition (composes with C1's confinement): an agent may
		// NEVER approve or reject its own proposed action. Return the specific,
		// audited approval-route denial here — before the general agent-scope
		// confinement below. Both would 403 an agent on an approval route (that
		// route is not under /v1/agent/*), so this neither widens nor narrows the
		// boundary; it names the reason precisely. tier 3 always terminates at a
		// human.
		//
		// LAYERING (verified, trust-the-substrate W1-ζ): for the agent-session
		// keys the system actually mints this arm is the SECOND line, not the
		// first. auth.Auth checks RBAC before calling authorizeScope, and
		// MintAgentSessionKey issues at auth.AgentSessionKeyRole == RoleRunner
		// while the approval routes require RoleOperator — so a genuine agent
		// token is already denied "insufficient permissions" by the role gate and
		// never reaches here. The arm is kept deliberately as defence in depth:
		// it is what still refuses an agent claim carried on an operator-or-above
		// key (a future minting path, a stamped scope, or any widening of the
		// approval routes' required role). Deleting it would make the
		// "tier 3 terminates at a human" invariant depend on one role constant.
		// api/middleware/auth_scope_approval_test.go pins BOTH layers.
		if isIncidentApprovalRoute(routePath) {
			return nil, echo.NewHTTPError(http.StatusForbidden, ApprovalAgentTokenDenyMessage)
		}
		// C1: confine the agent token to its own incident's /v1/agent/* surface.
		return authorizeAgentScope(c, svc, agentClaim, routePath)
	}

	scopeJobs, err := auth.ScopeJobs(scopeJSON)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusForbidden, "insufficient permissions")
	}
	if len(scopeJobs) == 0 {
		return &scopeAuditContext{}, nil
	}

	if strings.HasPrefix(routePath, "/v1/connectors") {
		return nil, echo.NewHTTPError(http.StatusForbidden, "connectors require an unscoped principal")
	}

	state := &scopeAuditContext{}

	switch routePath {
	case "/auth/whoami":
		// Whoami is IDENTITY, not resource access: every authenticated API-key
		// principal — scoped or not — is entitled to be told who it is. Without
		// this case a job-scoped key fell through to the trailing deny below, so
		// a scoped key got 403 on its own principal lookup and could never
		// complete the UI's api-key login (ui/src/lib/auth.ts requires a 200
		// whoami). No job is named by the route, so the audit context stays
		// empty — this grants no job access whatsoever.
		//
		// Agent-session tokens never reach here: the agent branch above returns
		// first, keeping an agent token confined to /v1/agent/* so it can never
		// complete a UI login.
		return state, nil
	case "/v1/jobs":
		switch c.Request().Method {
		case http.MethodGet:
			c.Set(ContextKeyAllowedJobAliases, append([]string(nil), scopeJobs...))
			return state, nil
		case http.MethodPost:
			aliases, err := parseJobAliasesForScope(c)
			if err != nil {
				return nil, echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
			}
			if len(aliases) != 1 || !auth.CheckScope(scopeJSON, aliases[0]) {
				return nil, echo.NewHTTPError(http.StatusForbidden, "insufficient permissions")
			}
			state.jobAliases = aliases
			return state, nil
		}
	case "/v1/jobdefs/apply":
		aliases, prune, err := parseApplyAliasesForScope(c)
		if err != nil {
			return nil, echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
		}
		if prune {
			return nil, echo.NewHTTPError(http.StatusForbidden, "insufficient permissions")
		}
		for _, alias := range aliases {
			if !auth.CheckScope(scopeJSON, alias) {
				return nil, echo.NewHTTPError(http.StatusForbidden, "insufficient permissions")
			}
		}
		state.jobAliases = aliases
		return state, nil
	case "/v1/lineage/impact":
		if c.Request().Method == http.MethodGet {
			return nil, echo.NewHTTPError(http.StatusForbidden, LineageImpactScopedDenyMessage)
		}
	case "/v1/contracts/graph":
		if c.Request().Method == http.MethodGet {
			return nil, echo.NewHTTPError(http.StatusForbidden, ContractsGraphScopedDenyMessage)
		}
	case "/v1/triggers/:id/fire":
		// A job-scoped runner key may fire a trigger only when every job the
		// trigger serves is one of its exact aliases (identity-and-access A6);
		// a trigger with no job has no owner and is refused. The handler
		// re-reads the jobs and confines each run's admission to the key's
		// scope, so a job added or moved later does not run.
		if c.Request().Method != http.MethodPost {
			break
		}
		triggerID, err := uuid.Parse(c.Param("id"))
		if err != nil {
			return nil, echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
		}
		aliases, err := svc.TriggerJobAliases(c.Request().Context(), triggerID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, echo.ErrNotFound
			}
			return nil, echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
		}
		if len(aliases) == 0 {
			return nil, echo.NewHTTPError(http.StatusForbidden, "insufficient permissions")
		}
		for _, alias := range aliases {
			// Exact stored alias: CheckScope trims, and a job stored as
			// "payroll " is not the key's "payroll".
			if !slices.Contains(scopeJobs, alias) {
				return nil, echo.NewHTTPError(http.StatusForbidden, "insufficient permissions")
			}
		}
		state.jobAliases = aliases
		return state, nil
	case "/v1/events":
		if c.Request().Method == http.MethodPost {
			// Ingest by a job-scoped runner key is delivered only to its exact
			// aliases (the handler's WriteConfinement scope; A6).
			return state, nil
		}
		if c.Request().Method == http.MethodGet {
			runIDRaw := strings.TrimSpace(c.QueryParam("run_id"))
			if runIDRaw == "" {
				return nil, echo.NewHTTPError(http.StatusForbidden, EventsScopedDenyMessage)
			}
			runID, err := uuid.Parse(runIDRaw)
			if err != nil {
				return nil, echo.NewHTTPError(http.StatusBadRequest, "invalid run_id")
			}
			jobAlias, err := svc.JobAliasByRunID(c.Request().Context(), runID)
			if err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return nil, echo.ErrNotFound
				}
				return nil, echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
			}
			if !auth.CheckScope(scopeJSON, jobAlias) {
				return nil, echo.NewHTTPError(http.StatusForbidden, "insufficient permissions")
			}
			state.jobAliases = []string{jobAlias}
			return state, nil
		}
	}

	if strings.HasPrefix(routePath, "/v1/jobs/:id") {
		jobAlias, err := resolveScopedJobAlias(c, svc, routePath)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, echo.ErrNotFound
			}
			return nil, echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
		}
		if !auth.CheckScope(scopeJSON, jobAlias) {
			return nil, echo.NewHTTPError(http.StatusForbidden, "insufficient permissions")
		}
		state.jobAliases = []string{jobAlias}
		return state, nil
	}

	return nil, echo.NewHTTPError(http.StatusForbidden, "insufficient permissions")
}

// authorizeAgentScope enforces an agent-session credential's incident binding.
// The token is valid ONLY for /v1/agent/incidents/:id/* routes whose :id equals
// the incident the token was minted for. Two negative properties are the whole
// point of this arm:
//
//   - Cross-route denial: any route outside /v1/agent/* (a normal job route, the
//     approval routes, /v1/lineage/impact, /v1/database/*) is 403'd — an agent
//     token can never act as a general principal.
//   - Cross-incident denial: an agent token minted for incident X hitting
//     incident Y's agent routes is 403'd — the incident id in the path must match
//     the frozen claim exactly.
//
// On success the frozen job allowlist is injected into the request context so the
// read-only context handlers can gate which jobs' logs/why/history the agent may
// pull. Server-side enforcement here is the security boundary; the agent's
// prompt is not.
//
// The injected allowlist keeps only the claim's jobs that are still in the
// incident's namespace (identity-and-access A8): the claim is frozen at
// incident open, and a job that moved out since must not stay readable. The
// incident-binding checks above always run first.
func authorizeAgentScope(c *echo.Context, svc *auth.Service, claim *auth.AgentClaimView, routePath string) (*scopeAuditContext, error) {
	if !strings.HasPrefix(routePath, AgentRoutePrefix) {
		return nil, echo.NewHTTPError(http.StatusForbidden, AgentScopeDenyMessage)
	}

	incParam := strings.TrimSpace(c.Param("id"))
	if incParam == "" {
		// An agent route with no incident id in scope is not part of the
		// per-incident tool surface an agent token may reach.
		return nil, echo.NewHTTPError(http.StatusForbidden, AgentScopeDenyMessage)
	}
	incID, err := uuid.Parse(incParam)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusBadRequest, "invalid incident id")
	}
	if incID != claim.IncidentID {
		return nil, echo.NewHTTPError(http.StatusForbidden, AgentScopeDenyMessage)
	}

	allowed, err := svc.AgentAllowedJobs(c.Request().Context(), claim.IncidentID, claim.Jobs)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, echo.ErrNotFound
		}
		return nil, echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	c.Set(ContextKeyAllowedJobAliases, allowed)
	return &scopeAuditContext{jobAliases: allowed}, nil
}

func resolveScopedJobAlias(c *echo.Context, svc *auth.Service, routePath string) (string, error) {
	ctx := c.Request().Context()

	switch {
	case strings.Contains(routePath, "/runs/:id/"):
		runID, err := uuid.Parse(c.Param("run_id"))
		if err != nil {
			return "", err
		}
		return svc.JobAliasByRunID(ctx, runID)
	case strings.Contains(routePath, "/runs/:id"):
		runID, err := uuid.Parse(c.Param("run_id"))
		if err != nil {
			return "", err
		}
		return svc.JobAliasByRunID(ctx, runID)
	case strings.Contains(routePath, "/backfills/:id"):
		backfillID, err := uuid.Parse(c.Param("backfill_id"))
		if err != nil {
			return "", err
		}
		return svc.JobAliasByBackfillID(ctx, backfillID)
	default:
		jobID, err := uuid.Parse(c.Param("id"))
		if err != nil {
			return "", err
		}
		return svc.JobAliasByID(ctx, jobID)
	}
}

func parseJobAliasesForScope(c *echo.Context) ([]string, error) {
	var payload struct {
		Alias string `json:"alias"`
	}
	if err := decodeScopedBody(c, &payload); err != nil {
		return nil, err
	}
	alias := strings.TrimSpace(payload.Alias)
	if alias == "" {
		return nil, errors.New("alias is required")
	}
	return []string{alias}, nil
}

func parseApplyAliasesForScope(c *echo.Context) ([]string, bool, error) {
	var payload struct {
		Definitions []struct {
			Metadata struct {
				Alias string `json:"alias"`
			} `json:"metadata"`
		} `json:"definitions"`
		Prune bool `json:"prune"`
	}
	if err := decodeScopedBody(c, &payload); err != nil {
		return nil, false, err
	}

	aliases := make([]string, 0, len(payload.Definitions))
	for _, def := range payload.Definitions {
		alias := strings.TrimSpace(def.Metadata.Alias)
		if alias == "" {
			return nil, false, errors.New("definition metadata.alias is required")
		}
		aliases = append(aliases, alias)
	}
	return aliases, payload.Prune, nil
}

func decodeScopedBody(c *echo.Context, target any) error {
	bodyBytes, err := readAndRestoreBody(c)
	if err != nil {
		return err
	}
	if len(bodyBytes) == 0 {
		return io.EOF
	}
	return json.Unmarshal(bodyBytes, target)
}

func readAndRestoreBody(c *echo.Context) ([]byte, error) {
	req := c.Request()
	if req.Body == nil {
		return nil, nil
	}

	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	c.SetRequest(req)
	return bodyBytes, nil
}
