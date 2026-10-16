package auth

import (
	"errors"
	"net/http"
	"time"

	"github.com/caesium-cloud/caesium/api/internal/auditlog"
	"github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type rotateKeyRequest struct {
	GracePeriod string `json:"grace_period"` // e.g. "24h"
}

func (ctrl *Controller) RotateKey(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid key id").Wrap(err)
	}

	var req rotateKeyRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	gracePeriod := 24 * time.Hour
	if req.GracePeriod != "" {
		gp, err := parseDuration(req.GracePeriod)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid grace_period").Wrap(err)
		}
		gracePeriod = gp
	}

	caller := middleware.GetPrincipal(c)
	if caller == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "authentication required")
	}

	resp, err := ctrl.service.RotateKeyAuthorized(c.Request().Context(), id, gracePeriod, caller.Subject, storedKeyAuthorizer(caller))
	if err != nil {
		if _, ok := errors.AsType[*authz.Denial](err); ok {
			return err
		}
		if undeclared, ok := errors.AsType[*iauth.UndeclaredNamespacesError](err); ok {
			return echo.NewHTTPError(http.StatusConflict, "cannot rotate the key: "+undeclared.Error())
		}
		switch {
		case errors.Is(err, iauth.ErrKeyNotFound):
			return echo.NewHTTPError(http.StatusNotFound, "api key not found")
		case errors.Is(err, iauth.ErrKeyRevoked):
			return echo.NewHTTPError(http.StatusConflict, "cannot rotate a revoked key")
		case errors.Is(err, iauth.ErrKeyExpired):
			return echo.NewHTTPError(http.StatusConflict, "cannot rotate an expired key")
		case errors.Is(err, iauth.ErrKeyNamespacesUnusable):
			return echo.NewHTTPError(http.StatusConflict, "cannot rotate a key whose namespaces are unusable; revoke it and create a new key")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to rotate api key").Wrap(err)
	}

	auditlog.LogFailure(ctrl.auditor.Log(iauth.AuditEntry{
		Actor:        caller.Subject,
		Action:       iauth.ActionKeyRotate,
		ResourceType: "api_key",
		ResourceID:   id.String(),
		SourceIP:     c.RealIP(),
		Outcome:      iauth.OutcomeSuccess,
		Namespace:    iauth.KeyAuditNamespace(resp.Key.Namespaces),
		Metadata: keyAuditMetadata(map[string]any{
			"new_key_id":   resp.Key.ID.String(),
			"grace_period": gracePeriod.String(),
		}, resp.Key),
	}))

	return c.JSON(http.StatusCreated, createKeyResponse{
		Key:    resp.Plaintext,
		APIKey: resp.Key,
	})
}
