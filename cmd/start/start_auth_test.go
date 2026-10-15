package start

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/stretchr/testify/require"
)

func TestHasTrustedProxyTLSPath(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "empty", raw: "", want: false},
		{name: "blank entries", raw: " , ", want: false},
		{name: "ip", raw: "127.0.0.1", want: true},
		{name: "cidr", raw: "10.0.0.0/24", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := hasTrustedProxyTLSPath(tt.raw)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestHasTrustedProxyTLSPathRejectsInvalidEntries(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "invalid only", raw: "not-a-cidr"},
		{name: "mixed valid and invalid", raw: "127.0.0.1, not-a-cidr"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := hasTrustedProxyTLSPath(tt.raw)
			require.Error(t, err)
			require.False(t, got)
		})
	}
}

func TestNewPolicySource(t *testing.T) {
	t.Run("no source without SSO, and the mapping is not parsed", func(t *testing.T) {
		for _, vars := range []env.Environment{
			{AuthMode: "api-key", AuthRoleMapping: "g=superuser"},
			{AuthMode: "none", AuthOIDCEnabled: true, AuthRoleMapping: "g=superuser"},
			{AuthMode: "", AuthLDAPEnabled: true, AuthRoleMapping: "g=superuser"},
		} {
			src, err := newPolicySource(vars)
			require.NoError(t, err)
			// == nil, not require.Nil: testify treats a typed nil *RoleMapper in
			// the interface as nil, which the SSO and middleware nil checks do not.
			require.True(t, src == nil, "a nil source must be an untyped nil interface, got %#v", src)
		}
	})

	t.Run("SSO enabled translates the env mapping", func(t *testing.T) {
		src, err := newPolicySource(env.Environment{
			AuthMode:        "api-key",
			AuthSAMLEnabled: true,
			AuthRoleMapping: "admins=admin;*=viewer",
			AuthDefaultRole: "runner",
		})
		require.NoError(t, err)
		require.NotNil(t, src)
		require.Equal(t, policy.Grants{"*": models.RoleAdmin}, src.Resolve([]string{" admins "}, ""))
		require.Equal(t, policy.Grants{"*": models.RoleViewer}, src.Resolve([]string{"other"}, "admin@example.com"))
	})

	t.Run("invalid mapping is an error", func(t *testing.T) {
		for _, vars := range []env.Environment{
			{AuthMode: "api-key", AuthOIDCEnabled: true, AuthRoleMapping: "g=superuser"},
			{AuthMode: "api-key", AuthOIDCEnabled: true, AuthDefaultRole: "root"},
		} {
			src, err := newPolicySource(vars)
			require.Error(t, err)
			require.True(t, src == nil, "got %#v", src)
		}
	})
}
