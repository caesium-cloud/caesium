package dataset

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	svc "github.com/caesium-cloud/caesium/api/rest/service/dataset"
	"github.com/caesium-cloud/caesium/internal/authz"
	runstore "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// releaseRequest is the body of POST /v1/datasets/holds/:id/release.
type releaseRequest struct {
	// Reason is the operator's justification. Required: a hold release
	// overrides the breaker, and "why" is the only thing the next reader has.
	Reason string `json:"reason"`
	// Tolerate maps an assertion kind to a Go duration window, the body form of
	// the CLI's `--tolerate <assertion>=<dur>`.
	//
	// RECORDED, NOT YET ENFORCED (v1): the entries are validated here and
	// persisted on the hold as evidence that the operator acked knowingly and
	// for how long, but no evaluator consults them yet — a breach inside a
	// tolerance window still re-opens the hold. Suppression is a follow-on. The
	// grammar is validated rather than accepted verbatim so the field cannot
	// quietly hold nonsense until the day something reads it.
	Tolerate map[string]string `json:"tolerate,omitempty"`
}

// Release handles POST /v1/datasets/holds/:id/release — the human ack that
// clears a dataset hold the breaker opened.
//
// FAIL-CLOSED. Caesium's default is CAESIUM_AUTH_MODE=none, which attaches no
// auth middleware at all, so under it every caller is anonymous. Releasing a
// hold is precisely the action that must be attributable — it overrides an
// automated safety decision — so with no auth mode active this returns 403
// NAMING the precondition rather than silently accepting an anonymous release.
// Holds on such a deployment still clear automatically through the clean_run
// path; only the manual override is refused.
func (ctrl *Controller) Release(c *echo.Context) error {
	id, err := uuid.Parse(strings.TrimSpace(c.Param("id")))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	if !svc.ReleaseAuthenticated() {
		return echo.NewHTTPError(http.StatusForbidden,
			"releasing a dataset hold requires an authenticated principal: set CAESIUM_AUTH_MODE=api-key or enable SSO")
	}

	var body releaseRequest
	if c.Request().Body != nil {
		if err := json.NewDecoder(c.Request().Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
		}
	}
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "reason is required")
	}

	tolerances, err := svc.ValidateTolerances(body.Tolerate)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	principal := releasePrincipal(c)
	if principal == "" {
		return echo.NewHTTPError(http.StatusForbidden,
			"releasing a dataset hold requires an authenticated principal")
	}

	result, err := newService(c.Request().Context()).ReleaseHold(svc.ReleaseHoldParams{
		HoldID:     id,
		Principal:  principal,
		SourceIP:   c.RealIP(),
		Reason:     reason,
		Tolerances: tolerances,
		// The owners' namespace(s) the auth middleware resolved for this
		// release (identity-and-access A3).
		AuditNamespace: authz.AuditNamespace(c),
		Caller:         authmw.GetPrincipal(c),
	})
	if err != nil {
		if mapped := writeError(err); mapped != nil {
			return mapped
		}
		switch {
		case errors.Is(err, svc.ErrHoldReleaseUnauthenticated):
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		case errors.Is(err, gorm.ErrRecordNotFound):
			return echo.ErrNotFound
		case errors.Is(err, runstore.ErrDatasetHoldNotActive):
			return echo.NewHTTPError(http.StatusConflict, "dataset hold is already released")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, result)
}

// releasePrincipal resolves the authenticated identity recorded as ReleasedBy.
// The auth-mode precondition above guarantees the middleware ran, so a missing
// principal here means the request was not authenticated and the release is
// refused rather than attributed to nobody.
func releasePrincipal(c *echo.Context) string {
	if p := authmw.GetPrincipal(c); p != nil {
		return strings.TrimSpace(p.Subject)
	}
	return ""
}
