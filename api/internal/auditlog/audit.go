package auditlog

import "github.com/caesium-cloud/caesium/pkg/log"

// LogFailure reports a best-effort audit write failure without changing its caller.
func LogFailure(err error) {
	if err != nil {
		log.Warn("failed to write audit log", "error", err)
	}
}
