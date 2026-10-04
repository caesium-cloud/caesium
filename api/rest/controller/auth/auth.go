package auth

import (
	"github.com/caesium-cloud/caesium/api/internal/auditlog"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
)

// Controller owns the dependencies required by the auth REST handlers.
type Controller struct {
	service *iauth.Service
	auditor *iauth.AuditLogger
}

// New constructs an auth controller with explicit dependencies.
func New(service *iauth.Service, auditor *iauth.AuditLogger) *Controller {
	return &Controller{
		service: service,
		auditor: auditor,
	}
}
