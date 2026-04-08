package auth

import (
	"net/http"
	"time"

	"github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type rotateKeyRequest struct {
	GracePeriod string `json:"grace_period"` // e.g. "24h"
}

func RotateKey(c *echo.Context) error {
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

	caller := middleware.GetAuthKey(c)
	actor := "unknown"
	if caller != nil {
		actor = caller.KeyPrefix
	}

	resp, err := Dependencies.Service.RotateKey(id, gracePeriod, actor)
	if err != nil {
		if err == iauth.ErrKeyNotFound {
			return echo.NewHTTPError(http.StatusNotFound, "api key not found")
		}
		if err == iauth.ErrKeyRevoked {
			return echo.NewHTTPError(http.StatusConflict, "cannot rotate a revoked key")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to rotate api key").Wrap(err)
	}

	logAuditFailure(Dependencies.Auditor.Log(iauth.AuditEntry{
		Actor:        actor,
		Action:       iauth.ActionKeyRotate,
		ResourceType: "api_key",
		ResourceID:   id.String(),
		SourceIP:     c.RealIP(),
		Outcome:      iauth.OutcomeSuccess,
		Metadata: map[string]interface{}{
			"new_key_id":   resp.Key.ID.String(),
			"grace_period": gracePeriod.String(),
		},
	}))

	return c.JSON(http.StatusCreated, createKeyResponse{
		Key:    resp.Plaintext,
		APIKey: resp.Key,
	})
}
