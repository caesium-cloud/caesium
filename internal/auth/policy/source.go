package policy

import "github.com/caesium-cloud/caesium/internal/models"

// Source resolves the grants of an authenticated identity, including any
// configured fallback role. An empty result means no_binding.
//
// Implementations are safe for concurrent use, are safe on a nil receiver, and
// return a fresh map on every call. Callers pass email as "" unless the
// identity provider verified or is trusted to assert it.
type Source interface {
	Resolve(groups []string, email string) Grants
}

// Static resolves against one immutable, validated policy revision.
type Static struct {
	Policy *AccessPolicy
	// DefaultRole is granted cluster-wide only when no binding matches.
	// "" (or an invalid role) means there is no fallback.
	DefaultRole models.Role
}

// Resolve applies the policy bindings, then the default role when nothing
// matched. Groups are matched byte-exact; nothing is trimmed here.
func (s *Static) Resolve(groups []string, email string) Grants {
	if s == nil {
		return Grants{}
	}
	grants := s.Policy.Resolve(groups, email)
	if len(grants) == 0 && models.ValidRole(string(s.DefaultRole)) {
		return Grants{ClusterNamespace: s.DefaultRole}
	}
	return grants
}
