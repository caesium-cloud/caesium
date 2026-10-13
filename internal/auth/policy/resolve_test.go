package policy_test

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
)

func TestResolve(t *testing.T) {
	t.Parallel()
	p, err := policy.Parse([]byte(header + `namespaces: {marketing: {}, finance: {}}
bindings:
  - subjects: {groups: [admins]}
    role: admin
    namespaces: ["*"]
  - subjects: {groups: [marketing-eng], users: [alice@example.com]}
    role: operator
    namespaces: [marketing]
  - subjects: {groups: [marketing-run]}
    role: runner
    namespaces: [marketing]
  - subjects: {groups: [finance-eng]}
    role: runner
    namespaces: [finance, default]
  - subjects: {groups: ["CN=Caesium Admins,OU=Groups,DC=example,DC=com"]}
    role: admin
    namespaces: [finance]
  - subjects: {groups: ["team-*"]}
    role: viewer
    namespaces: [default]
`))
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		groups []string
		email  string
		want   policy.Grants
	}{
		{"no match denies", []string{"other"}, "other@example.com", policy.Grants{}},
		{"no identity subjects denies", nil, "", policy.Grants{}},
		{"one group", []string{"marketing-eng"}, "", policy.Grants{"marketing": models.RoleOperator}},
		{"email only case insensitive", nil, "ALICE@EXAMPLE.COM", policy.Grants{"marketing": models.RoleOperator}},
		{"group or email", []string{"other"}, "alice@example.com", policy.Grants{"marketing": models.RoleOperator}},
		{"roles never downgraded", []string{"marketing-eng", "marketing-run", "marketing-run"}, "", policy.Grants{"marketing": models.RoleOperator}},
		{"independent namespace roles", []string{"marketing-eng", "finance-eng"}, "", policy.Grants{"marketing": models.RoleOperator, "finance": models.RoleRunner, "default": models.RoleRunner}},
		{"cluster grant is explicit", []string{"admins", "marketing-eng"}, "", policy.Grants{"*": models.RoleAdmin, "marketing": models.RoleOperator}},
		{"confined admin never escalates", []string{"CN=Caesium Admins,OU=Groups,DC=example,DC=com"}, "", policy.Grants{"finance": models.RoleAdmin}},
		{"groups are case sensitive", []string{"MARKETING-ENG"}, "", policy.Grants{}},
		{"groups are not trimmed", []string{" marketing-eng "}, "", policy.Grants{}},
		{"emails are not trimmed", nil, " alice@example.com ", policy.Grants{}},
		{"groups are not globs", []string{"team-one"}, "", policy.Grants{}},
		{"pattern-like group exact match", []string{"team-*"}, "", policy.Grants{"default": models.RoleViewer}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, p.Resolve(tc.groups, tc.email))
		})
	}
}

func TestResolveWildcardAndOrder(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		bindings := []policy.Binding{
			{Subjects: policy.Subjects{Groups: []string{"*"}}, Role: models.RoleViewer, Namespaces: []string{"*"}},
			{Subjects: policy.Subjects{Groups: []string{"eng"}}, Role: models.RoleAdmin, Namespaces: []string{"marketing"}},
			{Subjects: policy.Subjects{Groups: []string{"eng"}}, Role: models.RoleRunner, Namespaces: []string{"marketing"}},
		}
		if reverse {
			bindings[0], bindings[2] = bindings[2], bindings[0]
		}
		p := &policy.AccessPolicy{Bindings: bindings}
		require.Equal(t, policy.Grants{"*": models.RoleViewer}, p.Resolve(nil, "no-groups@example.com"))
		require.Equal(t, policy.Grants{"*": models.RoleViewer, "marketing": models.RoleAdmin}, p.Resolve([]string{"eng"}, ""))
		// Mutating a returned map cannot change policy or another request's grants.
		grants := p.Resolve([]string{"eng"}, "")
		grants["*"] = models.RoleAdmin
		require.Equal(t, policy.Grants{"*": models.RoleViewer}, p.Resolve(nil, "no-groups@example.com"))
	}
	var missing *policy.AccessPolicy
	require.Equal(t, policy.Grants{}, missing.Resolve([]string{"eng"}, "alice@example.com"))
}

func TestResolveEmailDoesNotFoldUnicodeIdentities(t *testing.T) {
	t.Parallel()
	p, err := policy.Parse([]byte(header + `bindings:
  - subjects: {users: [SAM@EXAMPLE.COM, KIM@EXAMPLE.COM]}
    role: admin
    namespaces: ["*"]
`))
	require.NoError(t, err)
	require.Equal(t, []string{"sam@example.com", "kim@example.com"}, p.Bindings[0].Subjects.Users)
	for _, email := range []string{"sam@example.com", "Sam@Example.Com", "kim@EXAMPLE.COM"} {
		require.Equal(t, policy.Grants{"*": models.RoleAdmin}, p.Resolve(nil, email))
	}
	for _, email := range []string{"ſam@example.com", "Kim@example.com", "ſAM@EXAMPLE.COM", "KIM@EXAMPLE.COM"} {
		require.Empty(t, p.Resolve(nil, email), "a Unicode case-fold equivalent must not acquire another email's grant")
	}
	// Programmatically constructed policies obey the same comparison contract.
	p.Bindings[0].Subjects.Users = []string{"ſam@example.com", "é@EXAMPLE.COM"}
	require.Equal(t, policy.Grants{"*": models.RoleAdmin}, p.Resolve(nil, "ſam@EXAMPLE.COM"))
	require.Equal(t, policy.Grants{"*": models.RoleAdmin}, p.Resolve(nil, "é@example.com"))
	require.Empty(t, p.Resolve(nil, "sam@example.com"))
	require.Empty(t, p.Resolve(nil, "É@example.com"))
}

func TestResolveGroupsRemainByteExact(t *testing.T) {
	t.Parallel()
	p, err := policy.Parse([]byte(header + `bindings:
  - subjects: {groups: [" team "]}
    role: runner
    namespaces: [default]
`))
	require.NoError(t, err)
	require.Empty(t, p.Resolve([]string{"team"}, ""))
	require.Empty(t, p.Resolve([]string{" TEAM "}, ""))
	require.Equal(t, policy.Grants{"default": models.RoleRunner}, p.Resolve([]string{" team "}, ""))
}
