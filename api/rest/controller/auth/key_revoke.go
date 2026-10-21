package auth

import (
	"errors"
	"net/http"

	"github.com/caesium-cloud/caesium/api/internal/auditlog"
	"github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

func (ctrl *Controller) RevokeKey(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid key id").Wrap(err)
	}

	caller := middleware.GetPrincipal(c)
	if caller == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "authentication required")
	}

	key, err := ctrl.service.RevokeKeyAuthorized(c.Request().Context(), id, storedKeyAuthorizer(caller))
	if err != nil {
		if _, ok := errors.AsType[*authz.Denial](err); ok {
			return err
		}
		if errors.Is(err, iauth.ErrKeyNotFound) {
			return echo.NewHTTPError(http.StatusNotFound, "api key not found")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to revoke api key").Wrap(err)
	}

	auditlog.LogFailure(ctrl.auditor.Log(iauth.AuditEntry{
		Actor:        caller.Subject,
		Action:       iauth.ActionKeyRevoke,
		ResourceType: "api_key",
		ResourceID:   id.String(),
		SourceIP:     c.RealIP(),
		Outcome:      iauth.OutcomeSuccess,
		Namespace:    iauth.KeyAuditNamespace(key.Namespaces),
		Metadata:     userKeyAuditMetadata(keyAuditMetadata(nil, key), key),
	}))

	return c.JSON(http.StatusOK, map[string]string{"status": "revoked"})
}
