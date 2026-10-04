// Package enginekind defines the engine names supported by Caesium.
package enginekind

// IsSupported reports whether engine is an exact supported engine name.
func IsSupported(engine string) bool {
	switch engine {
	case "docker", "podman", "kubernetes":
		return true
	default:
		return false
	}
}
