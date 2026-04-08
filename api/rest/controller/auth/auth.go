package auth

import (
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/pkg/log"
)

// Dependencies holds the shared dependencies for auth controllers.
// Set during route binding.
var Dependencies struct {
	Service *auth.Service
	Auditor *auth.AuditLogger
}

func logAuditFailure(err error) {
	if err != nil {
		log.Warn("failed to write audit log", "error", err)
	}
}
