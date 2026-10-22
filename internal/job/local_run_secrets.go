package job

import (
	jobdefruntime "github.com/caesium-cloud/caesium/internal/jobdef/runtime"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
)

// secretScope scopes the run's secret resolver to the run's persisted
// namespace, reading the access policy NOW: every allow-check and every
// attempt's resolution builds a fresh scope, so a policy reload applies to the
// next check rather than to the next run. A client process's run (`caesium
// dev`, `reproduce`, a no-server `run retry`) never waits for a server policy.
//
// Relative secret://k8s references read in the namespace mapping the task
// execution resolved (runner.kubeMapping), held for the whole execution.
func (l *localRun) secretScope(runner *atomRunner) *secret.ScopedResolver {
	namespace := l.runNamespace()
	if l.j != nil && l.j.clientProcess {
		return secret.ForClientNamespace(l.secretResolver, namespace)
	}
	kubeNamespace := ""
	if runner != nil {
		kubeNamespace = runner.kubeMapping.Namespace
	}
	return secret.ForNamespaceTarget(l.secretResolver, namespace, kubeNamespace)
}

// runNamespace is the run's persisted namespace.
func (l *localRun) runNamespace() string {
	if l.currentRun != nil {
		return l.currentRun.Namespace
	}
	return ""
}

// resolveKubernetesMapping reads the run namespace's Kubernetes mapping for
// one task execution onto its runner (jobdefruntime.KubernetesMapping),
// with any refusal in runner.kubeMappingErr. A client process applies no
// server policy.
func (l *localRun) resolveKubernetesMapping(runner *atomRunner) {
	client := l.j != nil && l.j.clientProcess
	runner.kubeMapping, runner.kubeMappingErr = jobdefruntime.KubernetesMapping(l.runNamespace(), runner.engineKind, client)
}

// recordKubernetesTarget records the execution's mapping on the TaskRun's
// descriptor (ref is the catalog task id of an unfanned step or an instance's
// TaskRun id). The local lane does not hold the row's descriptor, so a zero
// mapping reads it: a retry of a row an earlier execution ran under a mapping
// (since removed from the policy, or the policy file dropped) clears it.
func (l *localRun) recordKubernetesTarget(ref uuid.UUID, runner *atomRunner) {
	if err := l.store.RecordTaskKubernetesTarget(l.runID, ref, runner.kubeMapping, nil); err != nil {
		log.Warn("failed to record task kubernetes target on its descriptor", "run_id", l.runID, "task_ref", ref, "error", err)
	}
}

// checkSecretsAllowed is the before-cache allow-check for one task: every
// secret:// reference in the runner's step env must be allowed by the run
// namespace's secret rules before a cache lookup may satisfy the task. A
// namespace without rules returns at once.
func (l *localRun) checkSecretsAllowed(runner *atomRunner) error {
	if runner == nil {
		return nil
	}
	return jobdefruntime.CheckContainerSpecSecretsAllowed(l.secretScope(runner), runner.spec)
}
