package atom

import (
	"fmt"
	"strings"
)

// KubeTarget is where a Kubernetes atom runs: the pod namespace and the
// default service account the run's namespace maps to in the access policy
// (identity-and-access design §10.1). The zero value is the engine's
// configured namespace (CAESIUM_KUBERNETES_NAMESPACE) and no default service
// account. It is a plain value so the dispatch isolation context can carry it.
type KubeTarget struct {
	Namespace          string `json:"namespace,omitempty"`
	ServiceAccountName string `json:"serviceAccountName,omitempty"`
}

// IsZero reports whether t selects the engine defaults.
func (t KubeTarget) IsZero() bool {
	return t.Namespace == "" && t.ServiceAccountName == ""
}

// Ptr returns nil for the zero target, else a pointer to a copy, so optional
// fields (descriptor, cache identity) stay absent for the defaults.
func (t KubeTarget) Ptr() *KubeTarget {
	if t.IsZero() {
		return nil
	}
	out := t
	return &out
}

// QualifyRuntimeID returns the namespace-qualified runtime id "<ns>/<name>".
func QualifyRuntimeID(namespace, name string) string {
	return namespace + "/" + name
}

// ResolveRuntimeID splits a runtime id into the namespace and name it
// addresses. A qualified id ("<ns>/<name>") names its own namespace, and a
// non-empty requested namespace that differs is refused rather than silently
// addressing another namespace. An unqualified (legacy) id resolves in the
// requested namespace, else in fallback.
func ResolveRuntimeID(id, requested, fallback string) (namespace, name string, err error) {
	if ns, rest, ok := strings.Cut(id, "/"); ok {
		if ns == "" || rest == "" || strings.Contains(rest, "/") {
			return "", "", fmt.Errorf("malformed runtime id %q", id)
		}
		if requested != "" && requested != ns {
			return "", "", fmt.Errorf("runtime id %q is in namespace %q, not the requested %q", id, ns, requested)
		}
		return ns, rest, nil
	}
	if requested != "" {
		return requested, id, nil
	}
	return fallback, id, nil
}
