package policy

import (
	"slices"

	"github.com/caesium-cloud/caesium/internal/models"
)

// ClusterRole returns the cluster-wide grant verbatim, or "" when there is none.
// It is not normalized, so audit and metric labels keep their exact values.
func (g Grants) ClusterRole() models.Role {
	return g[ClusterNamespace]
}

// RoleIn returns the effective role in namespace ns: the higher of the grant in
// ns and the cluster grant. ns is used literally; "" is not mapped to the
// default namespace, so it yields the cluster role.
func (g Grants) RoleIn(ns string) models.Role {
	cluster := g[ClusterNamespace]
	if role := g[ns]; models.RoleLevel(role) > models.RoleLevel(cluster) {
		return role
	}
	return cluster
}

// AllowedNamespaces returns nil when the cluster grant is at least viewer, which
// means unrestricted. Otherwise it returns the sorted namespaces granting at
// least viewer. That slice is never nil, so empty grants allow no namespace.
func (g Grants) AllowedNamespaces() []string {
	if models.RoleLevel(g[ClusterNamespace]) >= models.RoleLevel(models.RoleViewer) {
		return nil
	}
	allowed := make([]string, 0, len(g))
	for ns, role := range g {
		if ns != ClusterNamespace && models.RoleLevel(role) >= models.RoleLevel(models.RoleViewer) {
			allowed = append(allowed, ns)
		}
	}
	slices.Sort(allowed)
	return allowed
}
