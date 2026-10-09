package policy_test

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/stretchr/testify/require"
)

func TestMatchGlob(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		pattern string
		target  string
		want    bool
	}{
		{"env/MARKETING_*", "env/MARKETING_TOKEN", true},
		{"env/MARKETING_*", "env/FINANCE_TOKEN", false},
		{"env/MARKETING_*", "env/MARKETING_", true},
		{"env/MARKETING_*", "env/MARKETING_TOKEN/other", false},
		{"env/MARKETING_*", "vault/MARKETING_TOKEN", false},
		{"vault/secret/data/marketing/*", "vault/secret/data/marketing/token", true},
		{"vault/secret/data/marketing/*", "vault/secret/data/marketing/sub/token", false},
		{"vault/secret/data/marketing/**", "vault/secret/data/marketing", true},
		{"vault/secret/data/marketing/**", "vault/secret/data/marketing/sub/token#field", true},
		{"vault/**/token", "vault/token", true},
		{"vault/**/token", "vault/secret/data/marketing/token", true},
		{"vault/**/token", "vault/secret/data/marketing/token/other", false},
		{"vault/**/token", "env/token", false},
		{"vault/**/**/token", "vault/token", true},
		{"vault/**/**/token", "vault/secret/token", true},
		{"vault/**/team/*/**/token", "vault/team/key/token", true},
		{"vault/**/team/*/**/token", "vault/secret/team/key/sub/token", true},
		{"vault/**/team/*/**/token", "vault/team/token", false},
		{"k8s/marketing-*", "k8s/marketing-creds", true},
		{"k8s/marketing-*", "k8s/caesium-marketing/marketing-creds", false},
		{"k8s/caesium-marketing/*", "k8s/finance/creds", false},
		{"env/TOKEN_?", "env/TOKEN_a", true},
		{"env/TOKEN_?", "env/TOKEN_ab", false},
		{"env/TOKEN_[a-z]", "env/TOKEN_b", true},
		{"env/TOKEN_[a-z]", "env/TOKEN_1", false},
		{"env/TOKEN_[^0-9]", "env/TOKEN_a", true},
		{"env/TOKEN_[^0-9]", "env/TOKEN_5", false},
		{`env/TOKEN_\*`, "env/TOKEN_*", true},
		{`env/TOKEN_\*`, "env/TOKEN_x", false},
		{"env/TOKEN_[*]", "env/TOKEN_*", true},
		{`vault/secret/data/star\**`, "vault/secret/data/star*token", true},
		{`vault/secret/data/star\**`, "vault/secret/data/startoken", false},
		{`vault/secret/data/star\**`, "vault/secret/data/star*token/other", false},
		{"vault/secret/data/[**]", "vault/secret/data/*", true},
		{"vault/secret/data/[**]", "vault/secret/data/token", false},
		{`vault/\**/token`, "vault/*suffix/token", true},
		{`vault/\**/token`, "vault/token", false},
		{`vault/\**/token`, "vault/*suffix/other/token", false},
		{"env/foo**", "env/foobar", true},
		{"env/foo**", "env/foobar/other", false},
		{"env/**foo", "env/prefixfoo", true},
		{"env/***", "env/token", true},
		{"env/***", "env/nested/token", false},
		{"env/token", "env/token", true},
		{"env/token", "env/TOKEN", false},
		{"env/*", "ENV/token", false},
		{"vault/**", "vault/secret/../finance/token", true},
		{"vault/marketing/*", "vault/marketing/../finance/token", false},
		{"vault/marketing/token", "vault/marketing/./token", false},
		{"vault/**", "vault/secret//token", false},
		{"vault/**", "vault/secret/", false},
		{"vault/**", "vault/", false},
		{"vault/**", "vault", false},
		{"vault/**", "secret://vault/secret", false},
	} {
		t.Run(tc.pattern+" matches "+tc.target, func(t *testing.T) {
			t.Parallel()
			require.NoError(t, policy.ValidateGlob(tc.pattern))
			matched, err := policy.MatchGlob(tc.pattern, tc.target)
			require.NoError(t, err)
			require.Equal(t, tc.want, matched)
		})
	}
}

func TestInvalidGlobs(t *testing.T) {
	t.Parallel()
	for _, pattern := range []string{
		"", "env", "env/", "/token", "*/token", "Env/token", "secret://env/token",
		"env//token", "env/token/", "env/[", "env/[]", "env/[z-a", `env/token\`,
	} {
		t.Run(pattern, func(t *testing.T) {
			t.Parallel()
			require.Error(t, policy.ValidateGlob(pattern))
			matched, err := policy.MatchGlob(pattern, "env/token")
			require.Error(t, err)
			require.False(t, matched)
		})
	}
}
