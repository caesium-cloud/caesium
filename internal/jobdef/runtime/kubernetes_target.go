package runtime

import (
	"errors"
	"fmt"
	"slices"

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
