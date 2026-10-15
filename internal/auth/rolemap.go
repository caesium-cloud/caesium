package auth

import (
	"fmt"
	"slices"
	"strings"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
)

// RoleMapper is the env-mapping policy source, used when no policy file is
// set. It translates CAESIUM_AUTH_ROLE_MAPPING and CAESIUM_AUTH_DEFAULT_ROLE
// into cluster-wide bindings, so the highest matched role wins at "*". The
// wildcard entry "*" matches every login; the default role is only a fallback
// when no explicit or wildcard mapping applies.
//
// The env mapping is deprecated in favour of the access policy file. This
// adapter alone trims incoming groups, as the env mapping always has; policy
// file groups are matched byte-exact.
type RoleMapper struct {
	static *policy.Static
}

// NewRoleMapper parses a semicolon-separated group=role mapping and optional
// default role. Entries split on the last '=' so LDAP DNs can be used as keys.
// A repeated group or wildcard keeps its last entry.
func NewRoleMapper(mapping, defaultRole string) (*RoleMapper, error) {
	byGroup := map[string]models.Role{}
	var wildcard *models.Role
	for entry := range strings.SplitSeq(mapping, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		eq := strings.LastIndex(entry, "=")
		if eq <= 0 || eq == len(entry)-1 {
			return nil, fmt.Errorf("invalid role mapping entry %q (want group=role)", entry)
		}
		group := strings.TrimSpace(entry[:eq])
		role := models.Role(strings.TrimSpace(entry[eq+1:]))
		if !models.ValidRole(string(role)) {
			return nil, fmt.Errorf("invalid role %q in mapping", role)
		}
		if group == "*" {
			r := role
			wildcard = &r
			continue
		}
		byGroup[group] = role
	}

	var fallback models.Role
	if dr := strings.TrimSpace(defaultRole); dr != "" {
		if !models.ValidRole(dr) {
			return nil, fmt.Errorf("invalid default role %q", dr)
		}
		fallback = models.Role(dr)
	}

	// One binding per distinct group, after the last-entry-wins dedupe above:
	// a binding per raw entry would let "g=admin;g=viewer" resolve to admin.
	groups := make([]string, 0, len(byGroup))
	for group := range byGroup {
		groups = append(groups, group)
	}
	slices.Sort(groups)
	bindings := make([]policy.Binding, 0, len(groups)+1)
	for _, group := range groups {
		bindings = append(bindings, clusterBinding(group, byGroup[group]))
	}
	if wildcard != nil {
		bindings = append(bindings, clusterBinding("*", *wildcard))
	}
	translated := &policy.AccessPolicy{
		APIVersion: policy.APIVersionV1,
		Kind:       policy.KindAccessPolicy,
		Namespaces: map[string]policy.Namespace{policy.DefaultNamespace: {}},
		Bindings:   bindings,
	}
	if err := translated.Validate(); err != nil {
		return nil, fmt.Errorf("translate role mapping: %w", err)
	}
	return &RoleMapper{static: &policy.Static{Policy: translated, DefaultRole: fallback}}, nil
}

func clusterBinding(group string, role models.Role) policy.Binding {
	return policy.Binding{
		Subjects:   policy.Subjects{Groups: []string{group}},
		Role:       role,
		Namespaces: []string{policy.ClusterNamespace},
	}
}

// Resolve returns the grants for groups: the highest mapped role at "*", the
// default role when nothing matched, or empty grants (login denied). Incoming
// groups are trimmed. The env mapping has no user bindings, so email never
// grants anything.
func (m *RoleMapper) Resolve(groups []string, _ string) policy.Grants {
	if m == nil || m.static == nil {
		return policy.Grants{}
	}
	trimmed := make([]string, 0, len(groups))
	for _, group := range groups {
		trimmed = append(trimmed, strings.TrimSpace(group))
	}
	return m.static.Resolve(trimmed, "")
}
