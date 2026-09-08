package dataset

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	svc "github.com/caesium-cloud/caesium/api/rest/service/dataset"
	runstore "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// releaseRequest is the body of POST /v1/datasets/holds/:id/release.
type releaseRequest struct {
	// Reason is the operator's justification. Required: a hold release
	// overrides the breaker, and "why" is the only thing the next reader has.
	Reason string `json:"reason"`
	// Tolerate maps an assertion name to a duration window, the body form of
	// the CLI's `--tolerate <assertion>=<dur>`.
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

	vars := env.Variables()
	if vars.AuthMode != "api-key" && !vars.SSOEnabled() {
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

	principal := releasePrincipal(c)
	if principal == "" {
		return echo.NewHTTPError(http.StatusForbidden,
			"releasing a dataset hold requires an authenticated principal")
	}

	result, err := svc.New(c.Request().Context()).ReleaseHold(svc.ReleaseHoldParams{
		HoldID:     id,
		Principal:  principal,
		SourceIP:   c.RealIP(),
		Reason:     reason,
		Tolerances: body.Tolerate,
	})
	if err != nil {
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
