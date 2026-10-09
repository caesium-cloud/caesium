package policy

import "github.com/caesium-cloud/caesium/internal/models"

// Grants maps namespaces to their highest explicitly granted role. A grant at
// "*" is cluster-wide; resolving never promotes a confined role to that grant.
type Grants map[string]models.Role

// Resolve computes grants for an authenticated identity. Exact group membership,
// the group wildcard, and ASCII case-insensitive email matches are alternatives. The
// caller handles an empty result as no_binding and owns any configured fallback.
func (p *AccessPolicy) Resolve(groups []string, email string) Grants {
	grants := make(Grants)
	if p == nil {
		return grants
	}
	groupSet := make(map[string]bool, len(groups))
	for _, group := range groups {
		groupSet[group] = true
	}
	email = foldASCII(email)
	for _, binding := range p.Bindings {
		if !binding.Subjects.matches(groupSet, email) {
			continue
		}
		for _, namespace := range binding.Namespaces {
			if models.RoleLevel(binding.Role) > models.RoleLevel(grants[namespace]) {
				grants[namespace] = binding.Role
			}
		}
	}
	return grants
}

func (s Subjects) matches(groups map[string]bool, email string) bool {
	for _, group := range s.Groups {
		if group == "*" || groups[group] {
			return true
		}
	}
	if email != "" {
		for _, user := range s.Users {
			if foldASCII(user) == email {
				return true
			}
		}
	}
	return false
}

// foldASCII preserves every non-ASCII byte so Unicode case-fold equivalents
// cannot acquire grants belonging to a different email address.
func foldASCII(value string) string {
	folded := []byte(value)
	for i, char := range folded {
		if char >= 'A' && char <= 'Z' {
			folded[i] = char + ('a' - 'A')
		}
	}
	return string(folded)
}
