package auth

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
)

func TestRoleMapperResolve(t *testing.T) {
	m, err := NewRoleMapper("caesium-admins=admin;data-eng=operator;*=viewer", "")
	require.NoError(t, err)

	r, ok := m.Resolve([]string{"data-eng"})
	require.True(t, ok)
	require.Equal(t, models.RoleOperator, r)

	r, ok = m.Resolve([]string{"data-eng", "caesium-admins"})
	require.True(t, ok)
	require.Equal(t, models.RoleAdmin, r)

	r, ok = m.Resolve([]string{"unknown"})
	require.True(t, ok)
	require.Equal(t, models.RoleViewer, r)
}

func TestRoleMapperDenyByDefault(t *testing.T) {
	m, err := NewRoleMapper("admins=admin", "")
	require.NoError(t, err)
	_, ok := m.Resolve([]string{"nobody"})
	require.False(t, ok)
}

func TestRoleMapperDefaultRole(t *testing.T) {
	m, err := NewRoleMapper("admins=admin", "viewer")
	require.NoError(t, err)
	r, ok := m.Resolve([]string{"nobody"})
	require.True(t, ok)
	require.Equal(t, models.RoleViewer, r)
}

func TestRoleMapperAcceptsRunnerRole(t *testing.T) {
	m, err := NewRoleMapper("job-runners=runner", "runner")
	require.NoError(t, err)

	r, ok := m.Resolve([]string{"job-runners"})
	require.True(t, ok)
	require.Equal(t, models.RoleRunner, r)

	r, ok = m.Resolve([]string{"nobody"})
	require.True(t, ok)
	require.Equal(t, models.RoleRunner, r)
}

func TestRoleMapperWildcardParticipatesInHighestRole(t *testing.T) {
	m, err := NewRoleMapper("admins=viewer;*=operator", "")
	require.NoError(t, err)
	r, ok := m.Resolve([]string{"admins"})
	require.True(t, ok)
	require.Equal(t, models.RoleOperator, r)
}

func TestRoleMapperDNWithEquals(t *testing.T) {
	m, err := NewRoleMapper("CN=Caesium Admins,OU=Groups,DC=example,DC=com=admin", "")
	require.NoError(t, err)
	r, ok := m.Resolve([]string{"CN=Caesium Admins,OU=Groups,DC=example,DC=com"})
	require.True(t, ok)
	require.Equal(t, models.RoleAdmin, r)
}

func TestRoleMapperRejectsBadRole(t *testing.T) {
	_, err := NewRoleMapper("g=superuser", "")
	require.Error(t, err)
}
