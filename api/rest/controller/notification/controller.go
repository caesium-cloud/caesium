package notification

import (
	"github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/labstack/echo/v5"
)

// Controller owns the dependencies required by the notification REST
// handlers. It follows the same shape as api/rest/controller/auth.Controller
// so that channel/policy mutations can be audited the same way api-key
// mutations are.
type Controller struct {
	auditor *auth.AuditLogger
}

// New constructs a notification controller with explicit dependencies.
func New(auditor *auth.AuditLogger) *Controller {
	return &Controller{auditor: auditor}
}

// logAuditFailure logs (but does not fail the request on) an error writing an
// audit entry, matching api/rest/controller/auth.logAuditFailure.
func logAuditFailure(err error) {
	if err != nil {
		log.Warn("failed to write audit log", "error", err)
	}
}

// auditActor extracts the caller's key prefix from the echo context for the
// audit "actor" field, defaulting to "unknown" — the same fallback used by
// api/rest/controller/auth's RevokeKey/RotateKey handlers.
func auditActor(c *echo.Context) string {
	if caller := middleware.GetAuthKey(c); caller != nil {
		return caller.KeyPrefix
	}
	return "unknown"
}
