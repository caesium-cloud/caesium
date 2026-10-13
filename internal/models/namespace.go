package models

// DefaultNamespace is the tenancy of jobs created before namespace ownership.
const DefaultNamespace = "default"

// NamespaceOrDefault interprets legacy empty ownership as the default namespace.
func NamespaceOrDefault(namespace string) string {
	if namespace == "" {
		return DefaultNamespace
	}
	return namespace
}
