package auth

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/api/internal/auditlog"
	"github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/labstack/echo/v5"
)

type createKeyRequest struct {
	Description string           `json:"description"`
	Role        string           `json:"role"`
	Scope       *models.KeyScope `json:"scope,omitempty"`
	// Namespaces confines the key's role to these namespaces. Omitted (or
	// null) mints a cluster-wide key, as before namespaces existed.
	Namespaces *[]string `json:"namespaces,omitempty"`
	ExpiresIn  string    `json:"expires_in,omitempty"` // e.g. "90d", "24h"
}

type createKeyResponse struct {
	Key    string         `json:"key"`
	APIKey *models.APIKey `json:"api_key"`
}

func (ctrl *Controller) CreateKey(c *echo.Context) error {
	var req createKeyRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	if !models.ValidRole(req.Role) {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid role: %s (must be admin, operator, runner, or viewer)", req.Role))
	}
	// A job list that names no job would be dropped as blank and leave the key
	// narrowed to nothing, i.e. unscoped: refuse it rather than fail open.
	if req.Scope != nil && req.Scope.Agent == nil && req.Scope.Jobs != nil && !namesAJob(req.Scope.Jobs) {
		return echo.NewHTTPError(http.StatusBadRequest, "scope.jobs must name at least one job alias; omit scope for a key that is not narrowed to jobs")
	}

	var expiresAt *time.Time
	if req.ExpiresIn != "" {
		dur, err := parseDuration(req.ExpiresIn)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid expires_in").Wrap(err)
		}
		t := time.Now().UTC().Add(dur)
		expiresAt = &t
	}

	stored, namespaces, err := auth.NormalizeKeyNamespaces(req.Namespaces)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidKeyNamespaces) {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error()).Wrap(err)
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to create api key").Wrap(err)
	}
	if undeclared := auth.UndeclaredKeyNamespaces(namespaces); len(undeclared) > 0 {
		return echo.NewHTTPError(http.StatusBadRequest, (&auth.UndeclaredNamespacesError{Namespaces: undeclared}).Error())
	}

	caller := middleware.GetPrincipal(c)
	if caller == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "authentication required")
	}

	requestedRole := models.Role(req.Role)
	if err := authorizeKeyMint(caller, requestedRole, namespaces); err != nil {
		return err
	}

	resp, err := ctrl.service.CreateKey(&auth.CreateKeyRequest{
		Description: req.Description,
		Role:        requestedRole,
		Scope:       req.Scope,
		Namespaces:  stored,
		CreatedBy:   caller.Subject,
		ExpiresAt:   expiresAt,
	})
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to create api key").Wrap(err)
	}

	auditlog.LogFailure(ctrl.auditor.Log(auth.AuditEntry{
		Actor:        caller.Subject,
		Action:       auth.ActionKeyCreate,
		ResourceType: "api_key",
		ResourceID:   resp.Key.ID.String(),
		SourceIP:     c.RealIP(),
		Outcome:      auth.OutcomeSuccess,
		Namespace:    auth.KeyAuditNamespace(resp.Key.Namespaces),
		Metadata: keyAuditMetadata(map[string]any{
			"role":        req.Role,
			"description": req.Description,
		}, resp.Key),
	}))

	return c.JSON(http.StatusCreated, createKeyResponse{
		Key:    resp.Plaintext,
		APIKey: resp.Key,
	})
}

// namesAJob reports whether aliases holds a non-blank job alias (blank ones
// are dropped when the scope is read).
func namesAJob(aliases []string) bool {
	for _, alias := range aliases {
		if strings.TrimSpace(alias) != "" {
			return true
		}
	}
	return false
}

// parseDuration parses durations like "90d", "24h", "30m".
func parseDuration(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("duration too short: %s", s)
	}

	suffix := s[len(s)-1]
	value := s[:len(s)-1]

	switch suffix {
	case 'd':
		var days int
		if _, err := fmt.Sscanf(value, "%d", &days); err != nil {
			return 0, err
		}
		return time.Duration(days) * 24 * time.Hour, nil
	default:
		return time.ParseDuration(s)
	}
}
