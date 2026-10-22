package kubernetes

import (
	"fmt"
	"strings"
)

// Kubernetes runtime ids are namespace-qualified, "<namespace>/<pod>"
// (identity-and-access design §10.1), so recovery, wait, stop, logs, stats and
// reaping address the pod's own namespace without reading the access policy.
// Docker and Podman ids are opaque and never pass through these helpers.

// qualifyRuntimeID returns the namespace-qualified runtime id "<ns>/<name>".
func qualifyRuntimeID(namespace, name string) string {
	return namespace + "/" + name
}

// resolveRuntimeID splits a runtime id into the namespace and name it
// addresses. A qualified id ("<ns>/<name>") names its own namespace, and a
// non-empty requested namespace that differs is refused rather than silently
// addressing another namespace. An unqualified (legacy) id resolves in the
// requested namespace, else in fallback.
func resolveRuntimeID(id, requested, fallback string) (namespace, name string, err error) {
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
