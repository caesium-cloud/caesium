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

// ApprovalAgentTokenDenyMessage is the 403 reason returned when a per-session
// agent token (Stream C1) attempts to approve or reject a tier-3 remediation
// action. An agent may NEVER approve its own proposed action — tier 3 always
// terminates at a human. Exported so the auth and integration tests assert
// against the single source of truth.
const ApprovalAgentTokenDenyMessage = "agent session tokens may not approve or reject remediation actions"

// isIncidentApprovalRoute reports whether routePath is one of the tier-3 approval
// decision routes. The path is already normalised (parametric segments collapsed
// to :id), so both approve and reject share this prefix:
//
//	POST /v1/incidents/:id/approvals/:id/approve
//	POST /v1/incidents/:id/approvals/:id/reject
func isIncidentApprovalRoute(routePath string) bool {
	return strings.HasPrefix(routePath, "/v1/incidents/:id/approvals/")
}

// scopeHasAgentClaim reports whether an API-key scope carries the per-session
// agent claim minted by the incident manager for an agent session (Stream C1).
// Agent tokens are detected STRUCTURALLY — by the presence of the agent claim in
// the scope JSON — rather than by a Go type, so the rejection holds regardless of
// the claim's internal shape and without a compile-time dependency on C1's field.
// A normal job-alias scope ({"jobs":[...]}) carries neither key, so legitimate
// operator/runner keys are never mistaken for agents. Both likely claim names
// from the design language ("agent claim type" / "agent session token") are
// accepted so the check catches C1's representation either way.
func scopeHasAgentClaim(scopeJSON []byte) bool {
	if len(scopeJSON) == 0 {
		return false
	}
	var probe struct {
		Agent        json.RawMessage `json:"agent"`
		AgentSession json.RawMessage `json:"agent_session"`
	}
	if err := json.Unmarshal(scopeJSON, &probe); err != nil {
		return false
	}
	return isPresentJSON(probe.Agent) || isPresentJSON(probe.AgentSession)
}

// isPresentJSON reports whether a raw JSON field is present and non-null.
func isPresentJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && string(trimmed) != "null"
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
	// D1 security precondition: reject agent session tokens on the tier-3 approval
	// routes OUTRIGHT. This runs BEFORE the unscoped early-return below so an agent
	// token whose frozen job allowlist happens to be empty (len(scopeJobs) == 0)
	// cannot slip through — the agent claim, not the job list, is what disqualifies
	// it. An agent may never approve or reject its own proposed action.
	if isIncidentApprovalRoute(routePath) && scopeHasAgentClaim(scopeJSON) {
		return nil, echo.NewHTTPError(http.StatusForbidden, ApprovalAgentTokenDenyMessage)
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
