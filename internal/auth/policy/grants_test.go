package policy_test

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
)

func TestGrantsRoleIn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		grants policy.Grants
		ns     string
		want   models.Role
	}{
		{"empty grants", policy.Grants{}, "marketing", ""},
		{"nil grants", nil, "marketing", ""},
		{"namespace only", policy.Grants{"marketing": models.RoleOperator}, "marketing", models.RoleOperator},
		{"namespace only elsewhere", policy.Grants{"marketing": models.RoleOperator}, "finance", ""},
		{"cluster only", policy.Grants{"*": models.RoleViewer}, "marketing", models.RoleViewer},
		{"namespace above cluster", policy.Grants{"*": models.RoleViewer, "marketing": models.RoleAdmin}, "marketing", models.RoleAdmin},
		{"cluster above namespace", policy.Grants{"*": models.RoleOperator, "marketing": models.RoleRunner}, "marketing", models.RoleOperator},
		{"cluster namespace literal", policy.Grants{"*": models.RoleRunner, "marketing": models.RoleAdmin}, "*", models.RoleRunner},
		{"empty namespace is not default", policy.Grants{"default": models.RoleAdmin, "*": models.RoleViewer}, "", models.RoleViewer},
		{"empty namespace without cluster", policy.Grants{"default": models.RoleAdmin}, "", ""},
		{"unknown namespace role never wins", policy.Grants{"*": models.RoleViewer, "marketing": "superuser"}, "marketing", models.RoleViewer},
		{"unknown cluster role is raw", policy.Grants{"*": "superuser"}, "marketing", "superuser"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, tc.grants.RoleIn(tc.ns))
		})
	}
	grants := policy.Grants{"*": models.RoleOperator, "marketing": models.RoleAdmin}
	require.Equal(t, grants.ClusterRole(), grants.RoleIn("*"))
}

func TestGrantsClusterRoleIsRaw(t *testing.T) {
	t.Parallel()
	require.Equal(t, models.Role(""), policy.Grants{}.ClusterRole())
	require.Equal(t, models.Role(""), policy.Grants(nil).ClusterRole())
	require.Equal(t, models.Role(""), policy.Grants{"marketing": models.RoleAdmin}.ClusterRole())
	require.Equal(t, models.RoleRunner, policy.Grants{"*": models.RoleRunner}.ClusterRole())
	require.Equal(t, models.Role(" Admin "), policy.Grants{"*": " Admin "}.ClusterRole())
}

func TestGrantsAllowedNamespaces(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		grants policy.Grants
		want   []string
	}{
		{"cluster viewer is unrestricted", policy.Grants{"*": models.RoleViewer}, nil},
		{"cluster admin is unrestricted", policy.Grants{"*": models.RoleAdmin, "marketing": models.RoleViewer}, nil},
		{"sorted subset", policy.Grants{"marketing": models.RoleOperator, "finance": models.RoleViewer, "default": models.RoleRunner}, []string{"default", "finance", "marketing"}},
		{"unknown roles excluded", policy.Grants{"marketing": "superuser", "finance": models.RoleViewer}, []string{"finance"}},
		{"invalid cluster role is not unrestricted", policy.Grants{"*": "superuser", "finance": models.RoleViewer}, []string{"finance"}},
		{"empty cluster role is not unrestricted", policy.Grants{"*": ""}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.grants.AllowedNamespaces()
			if tc.want == nil {
				require.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			require.Equal(t, tc.want, got)
		})
	}
}

// An empty grant set must never read as unrestricted: a nil slice means "all
// namespaces" to collection filters.
func TestGrantsAllowedNamespacesEmptyIsNotNil(t *testing.T) {
	t.Parallel()
	for _, grants := range []policy.Grants{nil, {}} {
		got := grants.AllowedNamespaces()
		require.NotNil(t, got)
		require.Empty(t, got)
	}
}
