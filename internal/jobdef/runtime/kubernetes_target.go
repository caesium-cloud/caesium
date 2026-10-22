package runtime

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
)

// ErrKubernetesTargetDenied is matched (errors.Is) by a refusal to run a
// Kubernetes task whose namespace's target cannot be known.
var ErrKubernetesTargetDenied = errors.New("kubernetes target denied by the access policy")

// KubernetesTargetError refuses a Kubernetes task: the run namespace is not
// declared by this node's access policy file, or the configured file never
// loaded. Its message carries secret.DeniedMarker so the incident classifier
// records the failure as a config failure, never transient_infra.
type KubernetesTargetError struct {
	Namespace string
	Reason    string
}

func (e *KubernetesTargetError) Error() string {
	return fmt.Sprintf("kubernetes target %s %q access policy: %s", secret.DeniedMarker, e.Namespace, e.Reason)
}

// Is makes every KubernetesTargetError match ErrKubernetesTargetDenied.
func (e *KubernetesTargetError) Is(target error) bool {
	return target == ErrKubernetesTargetDenied
}

// KubernetesMapping resolves a run namespace's Kubernetes mapping for one task
// execution (identity-and-access design §10.1): the access policy's
// namespaces.<ns>.kubernetes.{namespace, serviceAccountName}, read now from this
// server's policy. Callers resolve it once per execution, before the cache
// identity, and hold it across that execution's retries; the policy revision
// at execution start is authoritative.
//
// The zero value is no mapping: pods run in CAESIUM_KUBERNETES_NAMESPACE with
// no default service account, and relative secret://k8s references read in
// CAESIUM_JOBDEF_SECRETS_KUBE_NAMESPACE. A client process (caesium dev,
// reproduce) applies no server policy and always gets the zero value.
//
// A Kubernetes task whose namespace this node's policy file does not declare,
// or whose configured policy file never loaded, is refused (fail closed): the
// namespace it must run in cannot be known. Other engines only use the
// mapping for relative secret://k8s reads, which the secret rules already
// refuse in those cases, so they get the zero value.
func KubernetesMapping(namespace string, engine models.AtomEngine, client bool) (atom.KubeTarget, error) {
	if client {
		return atom.KubeTarget{}, nil
	}
	namespace = models.NamespaceOrDefault(namespace)
	kubernetesTask := engine == models.AtomEngineKubernetes
	holder, ok := secret.ServerPolicy()
	if !ok {
		if kubernetesTask {
			return atom.KubeTarget{}, &KubernetesTargetError{Namespace: namespace, Reason: "the configured access policy is not loaded"}
		}
		return atom.KubeTarget{}, nil
	}
	settings, declared := holder.Namespace(namespace)
	if !declared {
		if kubernetesTask && holder.FileBacked() {
			return atom.KubeTarget{}, &KubernetesTargetError{Namespace: namespace, Reason: "the namespace is not declared in this node's access policy"}
		}
		return atom.KubeTarget{}, nil
	}
	if settings.Kubernetes == nil {
		return atom.KubeTarget{}, nil
	}
	return atom.KubeTarget{
		Namespace:          settings.Kubernetes.Namespace,
		ServiceAccountName: settings.Kubernetes.ServiceAccountName,
	}, nil
}

// PodTarget is the pod target an engine request carries for a task of engine
// under mapping: the mapping for a Kubernetes task, nothing otherwise.
func PodTarget(engine models.AtomEngine, mapping atom.KubeTarget) atom.KubeTarget {
	if engine != models.AtomEngineKubernetes {
		return atom.KubeTarget{}
	}
	return mapping
}

// KubernetesIdentity returns the parts of a task's namespace mapping that enter
// its cache identity: the pod target of a Kubernetes task, and for any other
// engine the namespace its relative secret://k8s references read in, when the
// step has one. Both are empty without a mapping, so every unmapped identity
// is unchanged, and a remap re-keys exactly the tasks it changes.
func KubernetesIdentity(engine models.AtomEngine, mapping atom.KubeTarget, env map[string]string) (*atom.KubeTarget, string) {
	if engine == models.AtomEngineKubernetes {
		return mapping.Ptr(), ""
	}
	if mapping.Namespace != "" && secret.RelativeKubernetesReference(env) {
		return nil, mapping.Namespace
	}
	return nil, ""
}

// RecordedKubernetesMapping is the mapping a descriptor recorded for an
// execution; a descriptor without one (written before C1, or for an unmapped
// namespace) recorded the legacy default, the zero value.
func RecordedKubernetesMapping(recorded *atom.KubeTarget) atom.KubeTarget {
	if recorded == nil {
		return atom.KubeTarget{}
	}
	return *recorded
}

// KubernetesNamespaces is every Kubernetes namespace this server's pods may
// run in: CAESIUM_KUBERNETES_NAMESPACE plus each namespace the current access
// policy maps, sorted. A label-based reaper lists these (EngineListRequest);
// recovery of a known task never needs it, since its runtime id names its
// namespace.
func KubernetesNamespaces() []string {
	out := []string{env.Variables().KubernetesNamespace}
	out = append(out, mappedKubernetesNamespaces(policy.ProcessHolder())...)
	slices.Sort(out)
	return slices.Compact(out)
}

func mappedKubernetesNamespaces(holder *policy.Holder) []string {
	rev := holder.Current()
	if rev == nil || rev.Policy == nil {
		return nil
	}
	var out []string
	for _, ns := range rev.Policy.Namespaces {
		if ns.Kubernetes != nil && ns.Kubernetes.Namespace != "" {
			out = append(out, ns.Kubernetes.Namespace)
		}
	}
	return out
}

// IsolationContext is what a run owner resolves, from the run's persisted
// namespace and its own access policy, and carries to the worker it dispatches
// a task to (identity-and-access design §10.1, distributed mode): the
// Kubernetes target, the secret rules and the policy revision they came from.
// A worker that receives one never reads its own policy file; the effective
// fallbacks (the owner's default pod namespace and the namespace relative
// secret://k8s references read in) travel with it, so the worker's own
// environment can change neither the pod's namespace nor the secret object read.
//
// Only an owner serving a policy file sends one: with no file the dispatch is
// byte-identical to before.
type IsolationContext struct {
	// Namespace is the run namespace the context was resolved for.
	Namespace string `json:"namespace"`
	// PolicyRevision is the hash of the owner's policy file revision.
	PolicyRevision string `json:"policy_revision,omitempty"`
	// Kubernetes is the namespace's mapping (nil: none): the value C1 records
	// on the descriptor and folds into the cache identity.
	Kubernetes *atom.KubeTarget `json:"kubernetes,omitempty"`
	// DefaultKubernetesNamespace is the owner's CAESIUM_KUBERNETES_NAMESPACE,
	// the pod namespace when the mapping names none.
	DefaultKubernetesNamespace string `json:"default_kubernetes_namespace,omitempty"`
	// SecretReadNamespace is the Kubernetes namespace relative
	// secret://k8s/<secret>/<key> references read in: the mapping, else the
	// owner's CAESIUM_JOBDEF_SECRETS_KUBE_NAMESPACE.
	SecretReadNamespace string `json:"secret_read_namespace,omitempty"`
	// Secrets are the namespace's allow rules (nil: allow all; an empty list
	// denies all).
	Secrets *policy.SecretRules `json:"secrets,omitempty"`
	// SecretRuleKubeNamespace is the Kubernetes namespace a relative
	// k8s/<secret> rule means: the mapping, else the owner's
	// CAESIUM_KUBERNETES_NAMESPACE.
	SecretRuleKubeNamespace string `json:"secret_rule_kube_namespace,omitempty"`
	// Undeclared: the owner's policy file does not declare the namespace, so
	// every secret reference is denied and a Kubernetes task is refused.
	Undeclared bool `json:"undeclared,omitempty"`
}

// Default reports whether the context isolates nothing: no Kubernetes mapping,
// no secret rule and a declared namespace. Only a non-default context requires
// a worker that advertises the isolation capability.
func (c *IsolationContext) Default() bool {
	return c == nil || (c.Kubernetes == nil && c.Secrets == nil && !c.Undeclared)
}

// ResolveIsolationContext resolves namespace's isolation context from this
// process's access policy, reading one policy revision. carry is false when the
// process serves no policy file (no context is sent). ready is false while a
// configured policy file is not installed yet (startup): the caller must not
// dispatch, since the namespace's isolation cannot be known. It never blocks.
func ResolveIsolationContext(namespace string) (ctx IsolationContext, carry, ready bool) {
	namespace = models.NamespaceOrDefault(namespace)
	holder := policy.ProcessHolder()
	if holder == nil {
		if strings.TrimSpace(env.Variables().AuthPolicyFile) != "" {
			return IsolationContext{}, false, false
		}
		return IsolationContext{}, false, true
	}
	if !holder.FileBacked() {
		return IsolationContext{}, false, true
	}
	rev := holder.Current()
	vars := env.Variables()
	// The owner's effective defaults, normalized as its own engine and
	// resolver normalize them: an explicitly empty setting means "default",
	// while an empty field in the context would mean "the worker's own".
	podDefault := effectiveKubeNamespace(vars.KubernetesNamespace)
	ctx = IsolationContext{
		Namespace:                  namespace,
		DefaultKubernetesNamespace: podDefault,
		SecretReadNamespace:        effectiveKubeNamespace(vars.JobdefSecretsKubeNamespace),
		SecretRuleKubeNamespace:    podDefault,
	}
	if rev != nil {
		ctx.PolicyRevision = rev.Hash
	}
	settings, declared := rev.Namespace(namespace)
	if !declared {
		ctx.Undeclared = true
		return ctx, true, true
	}
	if settings.Kubernetes != nil {
		target := atom.KubeTarget{Namespace: settings.Kubernetes.Namespace, ServiceAccountName: settings.Kubernetes.ServiceAccountName}
		ctx.Kubernetes = target.Ptr()
		if target.Namespace != "" {
			ctx.SecretReadNamespace = target.Namespace
			ctx.SecretRuleKubeNamespace = target.Namespace
		}
	}
	if ctx.SecretRuleKubeNamespace == "" {
		ctx.SecretRuleKubeNamespace = "default"
	}
	ctx.Secrets = settings.Secrets
	return ctx, true, true
}

// effectiveKubeNamespace is a configured Kubernetes namespace as the engine
// and the secret resolver apply it: empty means "default".
func effectiveKubeNamespace(namespace string) string {
	if strings.TrimSpace(namespace) == "" {
		return "default"
	}
	return namespace
}

// KubernetesMapping is KubernetesMapping read from the context instead of this
// node's policy: the namespace's mapping, and a refusal for a Kubernetes task
// whose namespace the owner's policy file does not declare.
func (c *IsolationContext) KubernetesMapping(engine models.AtomEngine) (atom.KubeTarget, error) {
	if c.Undeclared {
		if engine == models.AtomEngineKubernetes {
			return atom.KubeTarget{}, &KubernetesTargetError{Namespace: models.NamespaceOrDefault(c.Namespace), Reason: "the namespace is not declared in the run owner's access policy"}
		}
		return atom.KubeTarget{}, nil
	}
	if c.Kubernetes == nil {
		return atom.KubeTarget{}, nil
	}
	return *c.Kubernetes, nil
}

// PodTarget is PodTarget with the owner's effective fallback: a Kubernetes
// task's pod runs in the mapped namespace, else the owner's default namespace,
// never the worker's own CAESIUM_KUBERNETES_NAMESPACE.
func (c *IsolationContext) PodTarget(engine models.AtomEngine, mapping atom.KubeTarget) atom.KubeTarget {
	target := PodTarget(engine, mapping)
	if engine == models.AtomEngineKubernetes && target.Namespace == "" {
		target.Namespace = c.DefaultKubernetesNamespace
	}
	return target
}

// SecretRules is the secret part of the context, for secret.ForIsolation.
func (c *IsolationContext) SecretRules() secret.IsolationRules {
	return secret.IsolationRules{
		Namespace:         c.Namespace,
		Rules:             c.Secrets,
		RuleKubeNamespace: c.SecretRuleKubeNamespace,
		Undeclared:        c.Undeclared,
	}
}
