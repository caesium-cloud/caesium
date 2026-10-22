package secret

import (
	"context"
	"errors"
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/stretchr/testify/require"
)

// TestForIsolationEnforcesTheOwnersRules: a dispatched task's scope enforces
// exactly the rules its owner sent and never this process's policy (here the
// process serves one that would allow everything, then one that would deny
// everything).
func TestForIsolationEnforcesTheOwnersRules(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	inner := mappingTestResolver(t)
	rules := IsolationRules{Namespace: "marketing", Rules: &policy.SecretRules{Allow: []string{"env/MARKETING_*"}}, RuleKubeNamespace: "caesium-marketing"}
	for _, local := range []*policy.Holder{nil, writeScopedPolicy(t, scopedPolicy), writeScopedPolicy(t, "apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  marketing:\n    secrets: {allow: []}\n")} {
		policy.SetProcessHolder(local)
		scope := ForIsolation(inner, "marketing", "caesium-marketing", rules, nil)
		require.NoError(t, scope.Allow("secret://env/MARKETING_TOKEN"))
		require.ErrorIs(t, scope.Allow("secret://env/FINANCE_TOKEN"), ErrSecretDenied)
		require.False(t, scope.AllowsAll())
	}

	// nil rules allow all; an empty list denies all; undeclared denies all.
	require.True(t, ForIsolation(inner, "finance", "default", IsolationRules{Namespace: "finance"}, nil).AllowsAll())
	require.ErrorIs(t, ForIsolation(inner, "locked", "default", IsolationRules{Namespace: "locked", Rules: &policy.SecretRules{Allow: []string{}}}, nil).Allow("secret://env/X"), ErrSecretDenied)
	err := ForIsolation(inner, "ghost", "default", IsolationRules{Namespace: "ghost", Undeclared: true}, nil).Allow("secret://env/X")
	require.ErrorIs(t, err, ErrSecretDenied)

	// A context resolved for another namespace is unusable: deny.
	err = ForIsolation(inner, "finance", "default", IsolationRules{Namespace: "marketing"}, nil).Allow("secret://env/X")
	require.ErrorIs(t, err, ErrSecretDenied)
	require.ErrorContains(t, err, denyUnavailable)
}

// TestForIsolationConsultsTheOwnerOnEveryCheck: with a source of the owner's
// current rules, every allow-check, resolution and verification reads it, so a
// revocation since the dispatch applies to the next operation, and an owner
// that cannot be asked denies (fail closed).
func TestForIsolationConsultsTheOwnerOnEveryCheck(t *testing.T) {
	inner := mappingTestResolver(t)
	t.Setenv("CAESIUM_ISOLATION_TEST_TOKEN", "v")
	allowed := IsolationRules{Namespace: "marketing", Rules: &policy.SecretRules{Allow: []string{"env/CAESIUM_ISOLATION_*"}}}
	current := allowed
	var fetchErr error
	calls := 0
	scope := ForIsolation(inner, "marketing", "", allowed, func() (IsolationRules, error) {
		calls++
		return current, fetchErr
	})
	value, err := scope.Resolve(context.Background(), "secret://env/CAESIUM_ISOLATION_TEST_TOKEN")
	require.NoError(t, err)
	require.Equal(t, "v", value)
	require.Equal(t, 1, calls)

	current = IsolationRules{Namespace: "marketing", Rules: &policy.SecretRules{Allow: []string{}}}
	_, err = scope.Resolve(context.Background(), "secret://env/CAESIUM_ISOLATION_TEST_TOKEN")
	require.ErrorIs(t, err, ErrSecretDenied, "a rule the owner revoked applies to the next read")
	require.NotContains(t, err.Error(), "CAESIUM_ISOLATION_TEST_TOKEN\"", "denials stay redacted")

	current, fetchErr = allowed, errors.New("owner unreachable")
	err = scope.Allow("secret://env/CAESIUM_ISOLATION_TEST_TOKEN")
	require.ErrorIs(t, err, ErrSecretDenied)
	require.ErrorContains(t, err, denyUnavailable)
	require.Equal(t, 3, calls)
}

// TestForIsolationNeverShortcutsPastTheOwner: a dispatch whose context had no
// rules still consults the owner — the scope never reports allow-all — so rules
// the owner added since, or an owner that cannot be asked, deny.
func TestForIsolationNeverShortcutsPastTheOwner(t *testing.T) {
	inner := mappingTestResolver(t)
	unrestricted := IsolationRules{Namespace: "finance"}
	require.True(t, ForIsolation(inner, "finance", "", unrestricted, nil).AllowsAll())

	added := ForIsolation(inner, "finance", "", unrestricted, func() (IsolationRules, error) {
		return IsolationRules{Namespace: "finance", Rules: &policy.SecretRules{Allow: []string{}}}, nil
	})
	require.False(t, added.AllowsAll())
	require.ErrorIs(t, added.Allow("secret://env/ANY"), ErrSecretDenied)

	unreachable := ForIsolation(inner, "finance", "", unrestricted, func() (IsolationRules, error) {
		return IsolationRules{}, errors.New("owner unreachable")
	})
	require.False(t, unreachable.AllowsAll())
	require.ErrorContains(t, unreachable.Allow("secret://env/ANY"), denyUnavailable)

	still := ForIsolation(inner, "finance", "", unrestricted, func() (IsolationRules, error) { return unrestricted, nil })
	require.NoError(t, still.Allow("secret://env/ANY"))
}

// TestForIsolationReadsRelativeKubernetesReferencesInTheOwnersNamespace: the
// secret object a relative secret://k8s reference reads is the one the owner
// named, even when this worker's resolver defaults elsewhere.
func TestForIsolationReadsRelativeKubernetesReferencesInTheOwnersNamespace(t *testing.T) {
	inner := mappingTestResolver(t) // the worker's resolver defaults to "default"
	rules := IsolationRules{Namespace: "marketing", Rules: &policy.SecretRules{Allow: []string{"k8s/token"}}, RuleKubeNamespace: "caesium-marketing"}
	scope := ForIsolation(inner, "marketing", "caesium-marketing", rules, nil)
	value, err := scope.Resolve(context.Background(), "secret://k8s/token/value")
	require.NoError(t, err)
	require.Equal(t, "from-caesium-marketing", value)

	// The owner's unmapped default is honoured too.
	open := ForIsolation(inner, "plain", "caesium-marketing-v2", IsolationRules{Namespace: "plain"}, nil)
	value, err = open.Resolve(context.Background(), "secret://k8s/token/value")
	require.NoError(t, err)
	require.Equal(t, "from-caesium-marketing-v2", value)

	// A rule expanded for one namespace never grants a read in another.
	elsewhere := ForIsolation(inner, "marketing", "default", rules, nil)
	_, err = elsewhere.Resolve(context.Background(), "secret://k8s/token/value")
	require.ErrorIs(t, err, ErrSecretDenied)
}
