package auth

import (
	"fmt"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
)

// RoleMapper resolves IdP groups to Caesium roles. Highest matched role wins.
type RoleMapper struct {
	byGroup     map[string]models.Role
	wildcard    *models.Role
	defaultRole *models.Role
}

// NewRoleMapper parses a semicolon-separated group=role mapping and optional
// default role. Entries split on the last '=' so LDAP DNs can be used as keys.
func NewRoleMapper(mapping, defaultRole string) (*RoleMapper, error) {
	m := &RoleMapper{byGroup: map[string]models.Role{}}
	for _, entry := range strings.Split(mapping, ";") {
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
			m.wildcard = &r
			continue
		}
		m.byGroup[group] = role
	}

	if dr := strings.TrimSpace(defaultRole); dr != "" {
		if !models.ValidRole(dr) {
			return nil, fmt.Errorf("invalid default role %q", dr)
		}
		r := models.Role(dr)
		m.defaultRole = &r
	}
	return m, nil
}

// Resolve returns the effective role for groups and whether login is allowed.
func (m *RoleMapper) Resolve(groups []string) (models.Role, bool) {
	var best models.Role
	matched := false
	for _, g := range groups {
		role, ok := m.byGroup[strings.TrimSpace(g)]
		if !ok {
			continue
		}
		if !matched || models.RoleLevel(role) > models.RoleLevel(best) {
			best = role
			matched = true
		}
	}
	if matched {
		return best, true
	}
	if m.wildcard != nil {
		return *m.wildcard, true
	}
	if m.defaultRole != nil {
		return *m.defaultRole, true
	}
	return "", false
}
