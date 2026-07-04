package middleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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

// EventsScopedDenyMessage is returned when a scoped principal attempts to open
// the global event stream without a run_id filter. Scoped event subscriptions
// must resolve to one run owner before any events are streamed.
const EventsScopedDenyMessage = "event stream requires an in-scope run_id for scoped principals"

// AgentRoutePrefix is the only route prefix an agent-session credential may
// reach. Everything outside it is denied outright for agent tokens.
const AgentRoutePrefix = "/v1/agent/"

// AgentScopeDenyMessage is the 403 reason returned when an agent-session token
// is used on any route outside its incident's /v1/agent/* tool surface (a
// non-agent route, or a different incident's agent route). Exported so the auth
// and integration tests assert against the single source of truth.
const AgentScopeDenyMessage = "agent session token is scoped to its own incident's /v1/agent/* tool surface"

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
	// /v1/agent/* routes and nothing else. This check must precede the
	// job-scope path because an agent key carries an empty job list — treating
	// it as "unrestricted" (the len==0 branch below) would be a privilege
	// escalation, letting an agent token reach every unscoped route.
	agentClaim, err := auth.DecodeAgentClaim(scopeJSON)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusForbidden, "insufficient permissions")
	}
	if agentClaim != nil {
		return authorizeAgentScope(c, agentClaim, routePath)
	}

	scopeJobs, err := auth.ScopeJobs(scopeJSON)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusForbidden, "insufficient permissions")
	}
	if len(scopeJobs) == 0 {
		return &scopeAuditContext{}, nil
	}

	state := &scopeAuditContext{}

	switch routePath {
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
	case "/v1/events":
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
func authorizeAgentScope(c *echo.Context, claim *auth.AgentClaimView, routePath string) (*scopeAuditContext, error) {
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

	allowed := append([]string(nil), claim.Jobs...)
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

func decodeScopedBody(c *echo.Context, target interface{}) error {
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
