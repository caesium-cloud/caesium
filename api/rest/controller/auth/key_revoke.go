package auth

import (
	"net/http"

	"github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

func (ctrl *Controller) RevokeKey(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid key id").Wrap(err)
	}

	if err := ctrl.service.RevokeKey(id); err != nil {
		if err == iauth.ErrKeyNotFound {
			return echo.NewHTTPError(http.StatusNotFound, "api key not found")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to revoke api key").Wrap(err)
	}

	caller := middleware.GetAuthKey(c)
	actor := "unknown"
	if caller != nil {
		actor = caller.KeyPrefix
	}

	logAuditFailure(ctrl.auditor.Log(iauth.AuditEntry{
		Actor:        actor,
		Action:       iauth.ActionKeyRevoke,
		ResourceType: "api_key",
		ResourceID:   id.String(),
		SourceIP:     c.RealIP(),
		Outcome:      iauth.OutcomeSuccess,
	}))

	return c.JSON(http.StatusOK, map[string]string{"status": "revoked"})
}
