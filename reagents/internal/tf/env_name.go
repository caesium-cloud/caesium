package tf

import "strings"

// NormalizeEnvName folds a step or output name into its environment suffix.
// It mirrors the root module's pkg/task rule without importing that module:
// the shared contract is the marker protocol. test/infra_deploy_test.go drives
// the real server and guards against divergence between the two modules.
func NormalizeEnvName(name string) string {
	return strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name))
}
