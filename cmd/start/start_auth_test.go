package start

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth"
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
			{AuthMode: "none"},
			{AuthMode: "api-key"},
		} {
			src, holder, err := newPolicySource(t.Context(), vars, nil)
			require.NoError(t, err)
			// == nil, not require.Nil: testify treats a typed nil *RoleMapper in
			// the interface as nil, which the SSO and middleware nil checks do not.
			require.True(t, src == nil, "a nil source must be an untyped nil interface, got %#v", src)
			// The holder only renders the empty policy; it is not the source.
			requireEnvHolder(t, holder, policy.EmptyPolicy(), "")
		}
	})

	t.Run("SSO enabled translates the env mapping", func(t *testing.T) {
		src, holder, err := newPolicySource(t.Context(), env.Environment{
			AuthMode:        "api-key",
			AuthSAMLEnabled: true,
			AuthRoleMapping: "admins=admin;*=viewer",
			AuthDefaultRole: "runner",
		}, nil)
		require.NoError(t, err)
		require.NotNil(t, src)
		// The same type master returned, so session and login behaviour is
		// byte-identical: the RoleMapper, not the holder.
		mapper, ok := src.(*auth.RoleMapper)
		require.True(t, ok, "no-file SSO source must stay the RoleMapper, got %T", src)
		require.Equal(t, policy.Grants{"*": models.RoleAdmin}, src.Resolve([]string{" admins "}, ""))
		require.Equal(t, policy.Grants{"*": models.RoleViewer}, src.Resolve([]string{"other"}, "admin@example.com"))

		view, fallback := mapper.Translated()
		requireEnvHolder(t, holder, view, models.RoleRunner)
		require.Equal(t, models.RoleRunner, fallback)
		// The env view delegates to the mapper, trimming included.
		require.Equal(t, src.Resolve([]string{" admins "}, ""), holder.Resolve([]string{" admins "}, ""))
	})

	t.Run("invalid mapping is an error", func(t *testing.T) {
		for _, vars := range []env.Environment{
			{AuthMode: "api-key", AuthOIDCEnabled: true, AuthRoleMapping: "g=superuser"},
			{AuthMode: "api-key", AuthOIDCEnabled: true, AuthDefaultRole: "root"},
		} {
			src, holder, err := newPolicySource(t.Context(), vars, nil)
			require.Error(t, err)
			require.True(t, src == nil, "got %#v", src)
			require.Nil(t, holder)
		}
	})
}

func requireEnvHolder(t *testing.T, holder *policy.Holder, view *policy.AccessPolicy, defaultRole models.Role) {
	t.Helper()
	require.NotNil(t, holder)
	require.False(t, holder.FileBacked())
	rev := holder.Current()
	require.NotNil(t, rev)
	require.Equal(t, policy.SourceEnv, rev.Source)
	require.Empty(t, rev.Hash)
	require.Equal(t, view, rev.Policy)
	require.Equal(t, defaultRole, holder.DefaultRole())
	_, err := holder.Reload(t.Context(), policy.ReloadRequest{Actor: "test"})
	require.ErrorIs(t, err, policy.ErrNoPolicyFile)
}

const startPolicyFile = `apiVersion: v1
kind: AccessPolicy
namespaces:
  marketing:
    secrets: {allow: ["env/MARKETING_*"]}
bindings:
  - subjects: {groups: [" spaced "]}
    role: operator
    namespaces: [marketing]
  - subjects: {groups: [admins]}
    role: admin
    namespaces: ["*"]
`

func writeStartPolicy(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestNewPolicySourceWithFile(t *testing.T) {
	path := writeStartPolicy(t, startPolicyFile)

	t.Run("api-key mode serves the file with or without SSO", func(t *testing.T) {
		for _, vars := range []env.Environment{
			{AuthMode: "api-key", AuthPolicyFile: path},
			{AuthMode: "api-key", AuthPolicyFile: path, AuthOIDCEnabled: true},
		} {
			src, holder, err := newPolicySource(t.Context(), vars, nil)
			require.NoError(t, err)
			require.NotNil(t, holder)
			require.True(t, holder.FileBacked())
			require.Same(t, holder, src.(*policy.Holder))
			require.Equal(t, policy.SourceFile, holder.Current().Source)
			// File groups are byte-exact: no trimming, unlike the env adapter.
			require.Equal(t, policy.Grants{"marketing": models.RoleOperator}, src.Resolve([]string{" spaced "}, ""))
			require.Empty(t, src.Resolve([]string{"spaced"}, ""))
			require.Equal(t, policy.Grants{"*": models.RoleAdmin}, src.Resolve([]string{"admins"}, ""))
		}
	})

	t.Run("default role keeps its fallback meaning", func(t *testing.T) {
		src, _, err := newPolicySource(t.Context(), env.Environment{AuthMode: "api-key", AuthPolicyFile: path, AuthDefaultRole: " viewer "}, nil)
		require.NoError(t, err)
		require.Equal(t, policy.Grants{"*": models.RoleViewer}, src.Resolve([]string{"nobody"}, ""))
		require.Equal(t, policy.Grants{"marketing": models.RoleOperator}, src.Resolve([]string{" spaced "}, ""))
	})

	t.Run("auth none validates the file but installs no source", func(t *testing.T) {
		src, holder, err := newPolicySource(t.Context(), env.Environment{AuthMode: "none", AuthPolicyFile: path}, nil)
		require.NoError(t, err)
		require.True(t, src == nil, "got %#v", src)
		require.True(t, holder.FileBacked())
		_, declared := holder.Namespace("marketing")
		require.True(t, declared)
	})

	t.Run("errors are fatal and name the file", func(t *testing.T) {
		invalid := writeStartPolicy(t, "apiVersion: v1\nkind: AccessPolicy\nbindings:\n  - subjects: {groups: [g]}\n    role: superuser\n    namespaces: [\"*\"]\n")
		missing := filepath.Join(t.TempDir(), "absent.yaml")
		for _, vars := range []env.Environment{
			{AuthMode: "api-key", AuthPolicyFile: invalid},
			{AuthMode: "none", AuthPolicyFile: invalid},
			{AuthMode: "api-key", AuthPolicyFile: missing},
		} {
			src, holder, err := newPolicySource(t.Context(), vars, nil)
			require.Error(t, err)
			require.Contains(t, err.Error(), vars.AuthPolicyFile)
			require.True(t, src == nil, "got %#v", src)
			require.Nil(t, holder)
		}
	})

	t.Run("file and role mapping together are an error naming both", func(t *testing.T) {
		_, _, err := newPolicySource(t.Context(), env.Environment{AuthMode: "api-key", AuthOIDCEnabled: true, AuthPolicyFile: path, AuthRoleMapping: "admins=admin"}, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "CAESIUM_AUTH_POLICY_FILE")
		require.Contains(t, err.Error(), "CAESIUM_AUTH_ROLE_MAPPING")
	})

	t.Run("an invalid default role is an error with a file", func(t *testing.T) {
		_, _, err := newPolicySource(t.Context(), env.Environment{AuthMode: "api-key", AuthPolicyFile: path, AuthDefaultRole: "root"}, nil)
		require.ErrorContains(t, err, "CAESIUM_AUTH_DEFAULT_ROLE")
	})
}
