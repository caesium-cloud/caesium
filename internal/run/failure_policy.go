package run

import "strings"

// NormalizeTaskFailurePolicy defaults unknown and empty values to halt.
func NormalizeTaskFailurePolicy(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "continue":
		return "continue"
	default:
		return "halt"
	}
}
