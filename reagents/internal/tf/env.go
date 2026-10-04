package tf

import (
	"os"
	"strings"
)

// EnvironmentWith copies the process environment and applies one override.
func EnvironmentWith(key, value string) map[string]string {
	env := make(map[string]string, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		env[k] = v
	}
	env[key] = value
	return env
}
