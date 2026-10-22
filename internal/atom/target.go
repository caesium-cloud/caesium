package atom

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
	return &t
}
