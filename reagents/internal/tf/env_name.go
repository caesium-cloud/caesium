package tf

import "strings"

// NormalizeEnvName folds a step or output name into its environment suffix.
func NormalizeEnvName(name string) string {
	return strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name))
}
