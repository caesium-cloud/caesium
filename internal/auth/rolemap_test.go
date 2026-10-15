package auth

import (
	"fmt"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
)

func TestRoleMapperResolve(t *testing.T) {
	m, err := NewRoleMapper("caesium-admins=admin;data-eng=operator;*=viewer", "")
	require.NoError(t, err)

	require.Equal(t, policy.Grants{"*": models.RoleOperator}, m.Resolve([]string{"data-eng"}, ""))
	require.Equal(t, policy.Grants{"*": models.RoleAdmin}, m.Resolve([]string{"data-eng", "caesium-admins"}, ""))
	require.Equal(t, policy.Grants{"*": models.RoleViewer}, m.Resolve([]string{"unknown"}, ""))
}

func TestRoleMapperDenyByDefault(t *testing.T) {
	m, err := NewRoleMapper("admins=admin", "")
	require.NoError(t, err)
	got := m.Resolve([]string{"nobody"}, "")
	require.NotNil(t, got)
	require.Empty(t, got)
}

func TestRoleMapperDefaultRole(t *testing.T) {
	m, err := NewRoleMapper("admins=admin", "viewer")
	require.NoError(t, err)
	require.Equal(t, policy.Grants{"*": models.RoleViewer}, m.Resolve([]string{"nobody"}, ""))
	require.Equal(t, policy.Grants{"*": models.RoleAdmin}, m.Resolve([]string{"admins"}, ""))
}

func TestRoleMapperAcceptsRunnerRole(t *testing.T) {
	m, err := NewRoleMapper("job-runners=runner", "runner")
	require.NoError(t, err)
	require.Equal(t, policy.Grants{"*": models.RoleRunner}, m.Resolve([]string{"job-runners"}, ""))
	require.Equal(t, policy.Grants{"*": models.RoleRunner}, m.Resolve([]string{"nobody"}, ""))
}

func TestRoleMapperWildcardParticipatesInHighestRole(t *testing.T) {
	m, err := NewRoleMapper("admins=viewer;*=operator", "")
	require.NoError(t, err)
	require.Equal(t, policy.Grants{"*": models.RoleOperator}, m.Resolve([]string{"admins"}, ""))
}

func TestRoleMapperDNWithEquals(t *testing.T) {
	m, err := NewRoleMapper("CN=Caesium Admins,OU=Groups,DC=example,DC=com=admin", "")
	require.NoError(t, err)
	require.Equal(t, policy.Grants{"*": models.RoleAdmin}, m.Resolve([]string{"CN=Caesium Admins,OU=Groups,DC=example,DC=com"}, ""))
}

func TestRoleMapperTrimsIncomingGroups(t *testing.T) {
	m, err := NewRoleMapper(" data-eng = operator ", "")
	require.NoError(t, err)
	require.Equal(t, policy.Grants{"*": models.RoleOperator}, m.Resolve([]string{" data-eng\t"}, ""))
}

func TestRoleMapperRejectsBadRole(t *testing.T) {
	for _, tc := range []struct {
		mapping     string
		defaultRole string
	}{
		{"g=superuser", ""},
		{"=admin", ""},
		{"g=", ""},
		{"g", ""},
		{"g=Admin", ""},
		{"g=admin", "superuser"},
	} {
		_, err := NewRoleMapper(tc.mapping, tc.defaultRole)
		require.Error(t, err, "mapping %q default %q", tc.mapping, tc.defaultRole)
	}
}

func TestRoleMapperIgnoresEmail(t *testing.T) {
	m, err := NewRoleMapper("admins=admin", "")
	require.NoError(t, err)
	require.Empty(t, m.Resolve(nil, "admin@example.com"))
	require.Empty(t, m.Resolve([]string{"admin@example.com"}, "admin@example.com"))
}

func TestRoleMapperTranslatedPolicyValidates(t *testing.T) {
	m, err := NewRoleMapper("b=viewer;a=admin;*=runner;a=operator;*=viewer", "viewer")
	require.NoError(t, err)
	require.NotNil(t, m.static)
	require.NoError(t, m.static.Policy.Validate())
	require.Equal(t, models.RoleViewer, m.static.DefaultRole)
	require.Contains(t, m.static.Policy.Namespaces, policy.DefaultNamespace)

	var groups []string
	for _, binding := range m.static.Policy.Bindings {
		require.Equal(t, []string{policy.ClusterNamespace}, binding.Namespaces)
		require.Len(t, binding.Subjects.Groups, 1)
		require.Empty(t, binding.Subjects.Users)
		groups = append(groups, fmt.Sprintf("%s=%s", binding.Subjects.Groups[0], binding.Role))
	}
	// Deduped last-entry-wins, sorted, wildcard last.
	require.Equal(t, []string{"a=operator", "b=viewer", "*=viewer"}, groups)
}

func TestRoleMapperNilReceiver(t *testing.T) {
	var m *RoleMapper
	got := m.Resolve([]string{"admins"}, "")
	require.NotNil(t, got)
	require.Empty(t, got)

	var src policy.Source = m
	require.Empty(t, src.Resolve([]string{"admins"}, ""))
	require.Empty(t, (&RoleMapper{}).Resolve([]string{"admins"}, ""))
}

// TestRoleMapperMatchesLegacyResolve pins the env adapter to the pre-A2
// RoleMapper.Resolve, copied below as an oracle: with no policy file the role
// every SSO user resolves must be unchanged.
func TestRoleMapperMatchesLegacyResolve(t *testing.T) {
	mappings := []string{
		"",
		"g=admin;g=viewer",
		"g=viewer;g=admin",
		"*=admin;*=viewer",
		"*=viewer;*=admin",
		"admins=viewer;*=operator",
		"admins=admin;*=viewer",
		"admins=admin;data-eng=operator;job-runners=runner;readers=viewer",
		" data-eng = operator ; ;readers=viewer",
		"CN=Caesium Admins,OU=Groups,DC=example,DC=com=admin;cn=ops,dc=x=operator",
		"a==admin;a=viewer",
	}
	defaults := []string{"", "viewer", "runner", " admin "}
	groupSets := [][]string{
		nil,
		{},
		{"g"},
		{" g "},
		{"admins"},
		{"admins", "data-eng"},
		{"data-eng", "job-runners", "readers"},
		{" data-eng "},
		{"data-eng\t"},
		{"nobody"},
		{"*"},
		{""},
		{"CN=Caesium Admins,OU=Groups,DC=example,DC=com"},
		{"cn=ops,dc=x"},
		{"a="},
		{"a"},
		{"Admins"},
	}
	for _, mapping := range mappings {
		for _, defaultRole := range defaults {
			legacy, err := newLegacyRoleMapper(mapping, defaultRole)
			require.NoError(t, err)
			current, err := NewRoleMapper(mapping, defaultRole)
			require.NoError(t, err)
			for _, groups := range groupSets {
				role, ok := legacy.resolve(groups)
				got := current.Resolve(groups, "someone@example.com")
				if !ok {
					require.Empty(t, got, "mapping %q default %q groups %q", mapping, defaultRole, groups)
					continue
				}
				require.Equal(t, policy.Grants{"*": role}, got, "mapping %q default %q groups %q", mapping, defaultRole, groups)
			}
		}
	}
}

// legacyRoleMapper is the RoleMapper parser and Resolve from before A2,
// verbatim, kept only as the oracle above.
type legacyRoleMapper struct {
	byGroup     map[string]models.Role
	wildcard    *models.Role
	defaultRole *models.Role
}

func newLegacyRoleMapper(mapping, defaultRole string) (*legacyRoleMapper, error) {
	m := &legacyRoleMapper{byGroup: map[string]models.Role{}}
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

func (m *legacyRoleMapper) resolve(groups []string) (models.Role, bool) {
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
	if m.wildcard != nil && (!matched || models.RoleLevel(*m.wildcard) > models.RoleLevel(best)) {
		best = *m.wildcard
		matched = true
	}
	if matched {
		return best, true
	}
	if m.defaultRole != nil {
		return *m.defaultRole, true
	}
	return "", false
}
