package job

import (
	jobdefruntime "github.com/caesium-cloud/caesium/internal/jobdef/runtime"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
)

// secretScope scopes the run's secret resolver to the run's persisted
// namespace, reading the access policy NOW: every allow-check and every
// attempt's resolution builds a fresh scope, so a policy reload applies to the
// next check rather than to the next run. A client process's run (`caesium
// dev`, `reproduce`, a no-server `run retry`) never waits for a server policy.
func (l *localRun) secretScope() *secret.ScopedResolver {
	namespace := ""
	if l.currentRun != nil {
		namespace = l.currentRun.Namespace
	}
	if l.j != nil && l.j.clientProcess {
		return secret.ForClientNamespace(l.secretResolver, namespace)
	}
	return secret.ForNamespace(l.secretResolver, namespace)
}

// checkSecretsAllowed is the before-cache allow-check for one task: every
// secret:// reference in the runner's step env must be allowed by the run
// namespace's secret rules before a cache lookup may satisfy the task. A
// namespace without rules returns at once.
func (l *localRun) checkSecretsAllowed(runner *atomRunner) error {
	if runner == nil {
		return nil
	}
	return jobdefruntime.CheckContainerSpecSecretsAllowed(l.secretScope(), runner.spec)
}
