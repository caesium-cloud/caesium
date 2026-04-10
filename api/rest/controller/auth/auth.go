package auth

import (
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/pkg/log"
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

func logAuditFailure(err error) {
	if err != nil {
		log.Warn("failed to write audit log", "error", err)
	}
}
