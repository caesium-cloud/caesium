package status

import "strings"

const (
	Pending   = "pending"
	Running   = "running"
	Succeeded = "succeeded"
	Failed    = "failed"
	Skipped   = "skipped"
)

// Normalize returns a lower-cased, trimmed status value.
func Normalize(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// IsTerminal reports whether a status indicates task completion.
func IsTerminal(value string) bool {
	switch Normalize(value) {
	case Succeeded, Failed, Skipped:
		return true
	default:
		return false
	}
}
