package middleware

import (
	"errors"
	"net/http"

	"github.com/caesium-cloud/caesium/api/internal/auditlog"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/labstack/echo/v5"
)

// LegacySecretAudit records one request gated by a deprecated shared secret
// (identity-and-access A6, auth.LegacyGate). Handlers call
// BeginLegacySecret first and defer Finish with their returned error, so a
// presented secret is audited whatever happens after it was accepted,
// including a failure after the request's work committed.
type LegacySecretAudit struct {
	c          *echo.Context
	auditor    *auth.AuditLogger
	secret     auth.LegacySecret
	principal  *auth.Principal
	namespaces []string
	metadata   map[string]any
}

// AuthEnabled reports whether this server authenticates its REST routes, as
// api/rest/bind mounts the auth middleware.
func AuthEnabled() bool {
	vars := env.Variables()
	return vars.AuthMode == "api-key" || vars.SSOEnabled()
}

// BeginLegacySecret applies auth.LegacyGate to the request. It returns the
// refusal as an HTTP error, or a non-nil audit only when a valid secret was
// presented (nil otherwise; Finish is nil-safe).
func BeginLegacySecret(c *echo.Context, auditor *auth.AuditLogger, secret auth.LegacySecret, expected string) (*LegacySecretAudit, error) {
	principal := GetPrincipal(c)
	decision := auth.LegacyGate(c.Request(), secret, expected, principal != nil, AuthEnabled())
	if decision.Status != 0 {
		return nil, echo.NewHTTPError(decision.Status, decision.Message)
	}
	if !decision.Used {
		return nil, nil
	}
	return &LegacySecretAudit{c: c, auditor: auditor, secret: secret, principal: principal, metadata: map[string]any{}}, nil
}

// SetNamespaces records the namespaces of the jobs the request reached.
func (a *LegacySecretAudit) SetNamespaces(namespaces []string) {
	if a == nil {
		return
	}
	a.namespaces = append([]string(nil), namespaces...)
}

// Set records one metadata value (delivery counts) for the row.
func (a *LegacySecretAudit) Set(key string, value any) {
	if a == nil {
		return
	}
	a.metadata[key] = value
}

// Finish writes the audit row. err is the handler's returned error: an
// error the handler returns is written to the client only after Finish runs,
// so the outcome is decided from it first and from the committed response
// status otherwise.
func (a *LegacySecretAudit) Finish(err error) {
	if a == nil || a.auditor == nil {
		return
	}
	entry := auth.LegacySecretAuditEntry(auth.LegacySecretUse{
		Secret:     a.secret,
		SourceIP:   a.c.RealIP(),
		Outcome:    legacySecretOutcome(a.c, err),
		Principal:  a.principal,
		Namespaces: a.namespaces,
		Metadata:   a.metadata,
	})
	auditlog.LogFailure(a.auditor.Log(entry))
}

func legacySecretOutcome(c *echo.Context, err error) string {
	status := 0
	if err != nil {
		if _, denied := errors.AsType[*authz.Denial](err); denied {
			return auth.OutcomeDenied
		}
		status = http.StatusInternalServerError
		if he, ok := errors.AsType[*echo.HTTPError](err); ok {
			status = he.Code
		}
	} else if resp, unwrapErr := echo.UnwrapResponse(c.Response()); unwrapErr == nil {
		status = resp.Status
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return auth.OutcomeDenied
	case status >= http.StatusBadRequest:
		return auth.OutcomeError
	default:
		return auth.OutcomeSuccess
	}
}
