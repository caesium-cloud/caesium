package policy_test

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
)

var _ policy.Source = (*policy.Static)(nil)

func TestStaticDefaultRoleOnlyWhenEmpty(t *testing.T) {
	t.Parallel()
	p, err := policy.Parse([]byte(header + `namespaces: {marketing: {}}
bindings:
  - subjects: {groups: [marketing-eng]}
    role: runner
    namespaces: [marketing]
  - subjects: {groups: [admins]}
    role: admin
    namespaces: ["*"]
`))
	require.NoError(t, err)
	src := &policy.Static{Policy: p, DefaultRole: models.RoleViewer}

	require.Equal(t, policy.Grants{"*": models.RoleViewer}, src.Resolve([]string{"nobody"}, ""))
	require.Equal(t, policy.Grants{"*": models.RoleViewer}, src.Resolve(nil, ""))
	// A confined match is not empty, so the default never joins it.
	require.Equal(t, policy.Grants{"marketing": models.RoleRunner}, src.Resolve([]string{"marketing-eng"}, ""))
	require.Equal(t, policy.Grants{"*": models.RoleAdmin}, src.Resolve([]string{"admins"}, ""))

	noDefault := &policy.Static{Policy: p}
	require.Empty(t, noDefault.Resolve([]string{"nobody"}, ""))
	require.NotNil(t, noDefault.Resolve([]string{"nobody"}, ""))
}

func TestStaticNilSafe(t *testing.T) {
	t.Parallel()
	var src *policy.Static
	got := src.Resolve([]string{"admins"}, "admin@example.com")
	require.NotNil(t, got)
	require.Empty(t, got)

	var iface policy.Source = src
	require.Empty(t, iface.Resolve([]string{"admins"}, ""))

	require.Empty(t, (&policy.Static{}).Resolve([]string{"admins"}, ""))
	require.Equal(t, policy.Grants{"*": models.RoleRunner},
		(&policy.Static{DefaultRole: models.RoleRunner}).Resolve([]string{"admins"}, ""))
}

func TestStaticInvalidDefaultIgnored(t *testing.T) {
	t.Parallel()
	for _, role := range []models.Role{"", "superuser", " viewer", "Viewer"} {
		got := (&policy.Static{DefaultRole: role}).Resolve([]string{"anyone"}, "")
		require.NotNil(t, got)
		require.Empty(t, got, "default role %q must not grant", role)
	}
}

func TestStaticGroupsByteExact(t *testing.T) {
	t.Parallel()
	p, err := policy.Parse([]byte(header + `bindings:
  - subjects: {groups: [data-eng]}
    role: operator
    namespaces: ["*"]
`))
	require.NoError(t, err)
	src := &policy.Static{Policy: p}
	require.Equal(t, policy.Grants{"*": models.RoleOperator}, src.Resolve([]string{"data-eng"}, ""))
	require.Empty(t, src.Resolve([]string{" data-eng "}, ""))
	require.Empty(t, src.Resolve([]string{"Data-Eng"}, ""))
}

func TestStaticReturnsFreshMaps(t *testing.T) {
	t.Parallel()
	src := &policy.Static{DefaultRole: models.RoleViewer}
	first := src.Resolve(nil, "")
	first["*"] = models.RoleAdmin
	require.Equal(t, policy.Grants{"*": models.RoleViewer}, src.Resolve(nil, ""))
}
