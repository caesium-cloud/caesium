package middleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// ContextKeyAllowedJobAliases stores the scoped job aliases available to list endpoints.
const ContextKeyAllowedJobAliases = "auth.allowed_job_aliases"

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

func authorizeScope(c *echo.Context, svc *auth.Service, key *models.APIKey, routePath string) (*scopeAuditContext, error) {
	scopeJobs, err := auth.ScopeJobs(key.Scope)
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
			if len(aliases) != 1 || !auth.CheckScope(key.Scope, aliases[0]) {
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
			if !auth.CheckScope(key.Scope, alias) {
				return nil, echo.NewHTTPError(http.StatusForbidden, "insufficient permissions")
			}
		}
		state.jobAliases = aliases
		return state, nil
	}

	if strings.HasPrefix(routePath, "/v1/jobs/:id") {
		jobAlias, err := resolveScopedJobAlias(c, svc, routePath)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, echo.ErrNotFound
			}
			return nil, echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
		}
		if !auth.CheckScope(key.Scope, jobAlias) {
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
