package auth

import (
	"errors"
	"net/http"
	"time"

	"github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/labstack/echo/v5"
)

// PolicyResponse is the body of GET /v1/auth/policy: the effective access
// policy, where it came from, and the last reload attempt.
type PolicyResponse struct {
	// Source is "file" (CAESIUM_AUTH_POLICY_FILE) or "env" (the translated
	// CAESIUM_AUTH_ROLE_MAPPING, or the empty policy when SSO is off).
	Source string `json:"source"`
	Path   string `json:"path,omitempty"`
	// Revision is the hex SHA-256 of the served file; "" for the env source.
	Revision    string               `json:"revision"`
	LoadedAt    time.Time            `json:"loaded_at"`
	DefaultRole models.Role          `json:"default_role"`
	Warnings    []string             `json:"warnings"`
	LastReload  *PolicyReloadAttempt `json:"last_reload"`
	Policy      *policy.AccessPolicy `json:"policy"`
}

// PolicyReloadAttempt is the most recent reload attempt, so drift and
// failures are visible; null before the first attempt.
type PolicyReloadAttempt struct {
	At       time.Time `json:"at"`
	Trigger  string    `json:"trigger"`
	OK       bool      `json:"ok"`
	Changed  bool      `json:"changed"`
	Revision string    `json:"revision,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// PolicyReloadResponse is the 200 body of POST /v1/auth/policy/reload.
type PolicyReloadResponse struct {
	Changed          bool      `json:"changed"`
	Revision         string    `json:"revision"`
	PreviousRevision string    `json:"previous_revision"`
	LoadedAt         time.Time `json:"loaded_at"`
	Warnings         []string  `json:"warnings"`
}

// PolicyReloadFailure is the 422 body of POST /v1/auth/policy/reload: the
// validation (or read) error and the revision that stays in force.
type PolicyReloadFailure struct {
	Message  string `json:"message"`
	Revision string `json:"revision"`
}

// GetPolicy returns the effective access policy (cluster admin).
func (ctrl *Controller) GetPolicy(c *echo.Context) error {
	holder := ctrl.service.PolicyHolder()
	rev := holder.Current()
	if rev == nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "access policy holder is not configured")
	}
	resp := PolicyResponse{
		Source:      rev.Source,
		Path:        holder.Path(),
		Revision:    rev.Hash,
		LoadedAt:    rev.LoadedAt,
		DefaultRole: holder.DefaultRole(),
		Warnings:    nonNilStrings(rev.Warnings),
		Policy:      renderPolicy(rev.Policy),
	}
	if attempt := holder.LastReload(); attempt != nil {
		resp.LastReload = &PolicyReloadAttempt{
			At:       attempt.At,
			Trigger:  attempt.Trigger,
			OK:       attempt.OK,
			Changed:  attempt.Changed,
			Revision: attempt.Revision,
			Error:    attempt.Error,
		}
	}
	return c.JSON(http.StatusOK, resp)
}

// ReloadPolicy re-reads the policy file synchronously (cluster admin). An
// invalid or unreadable file answers 422 and keeps the served revision; with
// no file configured it answers 409. Every attempt is audited by the holder.
func (ctrl *Controller) ReloadPolicy(c *echo.Context) error {
	holder := ctrl.service.PolicyHolder()
	if holder.Current() == nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "access policy holder is not configured")
	}
	if !holder.FileBacked() {
		return echo.NewHTTPError(http.StatusConflict, policy.ErrNoPolicyFile.Error())
	}

	actor := "unknown"
	if principal := middleware.GetPrincipal(c); principal != nil && principal.Subject != "" {
		actor = principal.Subject
	}
	result, err := holder.Reload(c.Request().Context(), policy.ReloadRequest{Actor: actor, SourceIP: c.RealIP()})
	if errors.Is(err, policy.ErrNoPolicyFile) {
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	}
	if err != nil {
		retained := ""
		if result.Current != nil {
			retained = result.Current.Hash
		}
		return c.JSON(http.StatusUnprocessableEntity, PolicyReloadFailure{Message: err.Error(), Revision: retained})
	}

	resp := PolicyReloadResponse{
		Changed:  result.Changed,
		Revision: result.Current.Hash,
		LoadedAt: result.Current.LoadedAt,
		Warnings: nonNilStrings(result.Current.Warnings),
	}
	if result.Previous != nil {
		resp.PreviousRevision = result.Previous.Hash
	}
	return c.JSON(http.StatusOK, resp)
}

// renderPolicy returns a shallow copy whose collections render as {} and []
// rather than null.
func renderPolicy(p *policy.AccessPolicy) *policy.AccessPolicy {
	if p == nil {
		return policy.EmptyPolicy()
	}
	out := *p
	if out.Namespaces == nil {
		out.Namespaces = map[string]policy.Namespace{}
	}
	if out.Bindings == nil {
		out.Bindings = []policy.Binding{}
	}
	return &out
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
