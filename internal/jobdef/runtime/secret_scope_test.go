package runtime

import (
	"context"
	"sync"
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/stretchr/testify/require"
)

func TestCheckContainerSpecSecretsAllowed(t *testing.T) {
	t.Setenv("C4_RUNTIME_TOKEN", "value")
	inner := secret.NewMultiResolver(map[string]secret.Resolver{"env": secret.NewEnvResolver()})
	spec := container.Spec{Env: map[string]string{
		"PLAIN": "plain",
		"B":     "secret://env/C4_RUNTIME_TOKEN",
		"A":     "secret://env/C4_RUNTIME_TOKEN?name=FINANCE_TOKEN",
	}}

	allowAll := secret.NewScopedResolver(inner, "marketing", nil, "")
	require.NoError(t, CheckContainerSpecSecretsAllowed(allowAll, spec))
	require.NoError(t, CheckContainerSpecSecretsAllowed(allowAll, container.Spec{}))

	scoped := secret.NewScopedResolver(inner, "marketing", &policy.SecretRules{Allow: []string{"env/C4_*"}}, "")
	err := CheckContainerSpecSecretsAllowed(scoped, spec)
	require.ErrorIs(t, err, secret.ErrSecretDenied)
	// Keys are checked in order: A (the ?name= escape attempt) first.
	require.EqualError(t, err, `env A: secret env/FINANCE_TOKEN/… is not allowed by namespace "marketing" secret rules: no rule matches`)
	require.NoError(t, CheckContainerSpecSecretsAllowed(scoped, container.Spec{Env: map[string]string{"B": spec.Env["B"], "PLAIN": "x"}}))

	var none *secret.ScopedResolver
	require.ErrorIs(t, CheckContainerSpecSecretsAllowed(none, spec), secret.ErrSecretDenied, "a missing scope never allows")
}

func TestResolveContainerSpecSecretsRedactsDenials(t *testing.T) {
	t.Setenv("C4_RUNTIME_TOKEN", "value")
	inner := secret.NewMultiResolver(map[string]secret.Resolver{"env": secret.NewEnvResolver()})
	scoped := secret.NewScopedResolver(inner, "marketing", &policy.SecretRules{Allow: []string{}}, "")
	_, _, err := ResolveContainerSpecSecretsWithIdentities(context.Background(), scoped, container.Spec{Env: map[string]string{"TOKEN": "secret://env/C4_RUNTIME_TOKEN"}})
	require.ErrorIs(t, err, secret.ErrSecretDenied)
	require.EqualError(t, err, `resolve env TOKEN: secret env/C4_RUNTIME_TOKEN/… is not allowed by namespace "marketing" secret rules: no rule matches`)

	// Other failures keep their pre-C4 wording, the reference included.
	_, _, err = ResolveContainerSpecSecretsWithIdentities(context.Background(), inner, container.Spec{Env: map[string]string{"TOKEN": "secret://env/C4_RUNTIME_UNSET"}})
	require.EqualError(t, err, "resolve env TOKEN from secret://env/C4_RUNTIME_UNSET: environment variable C4_RUNTIME_UNSET not set")

	resolved, identities, err := ResolveContainerSpecSecretsWithIdentities(context.Background(), secret.NewScopedResolver(inner, "marketing", nil, ""), container.Spec{Env: map[string]string{"TOKEN": "secret://env/C4_RUNTIME_TOKEN"}})
	require.NoError(t, err)
	require.Equal(t, "value", resolved.Env["TOKEN"])
	require.Len(t, identities, 1)
}

// revokeOnFirstRead revokes the namespace rule while the first reference of a
// resolution is being read.
type revokeOnFirstRead struct {
	secret.Resolver
	once   sync.Once
	revoke func()
}

func (r *revokeOnFirstRead) ResolveWithIdentity(ctx context.Context, ref string) (string, secret.Identity, error) {
	r.once.Do(r.revoke)
	return r.Resolver.ResolveWithIdentity(ctx, ref)
}

// TestTwoReferenceResolutionRereadsThePolicy: the policy is reloaded to deny
// while the first of two references is read; the second is then checked
// against the reloaded rules, so the resolution fails denied instead of
// reading it. Not parallel: it installs the process holder.
func TestTwoReferenceResolutionRereadsThePolicy(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	t.Setenv("C4_RUNTIME_A", "a")
	t.Setenv("C4_RUNTIME_B", "b")
	rules := func(allow ...string) *policy.Holder {
		view := policy.EmptyPolicy()
		view.Namespaces["marketing"] = policy.Namespace{Secrets: &policy.SecretRules{Allow: append([]string{}, allow...)}}
		return policy.NewEnvHolder(view, "", nil)
	}
	policy.SetProcessHolder(rules("env/C4_RUNTIME_*"))
	inner := &revokeOnFirstRead{
		Resolver: secret.NewMultiResolver(map[string]secret.Resolver{"env": secret.NewEnvResolver()}),
		revoke:   func() { policy.SetProcessHolder(rules()) },
	}
	spec := container.Spec{Env: map[string]string{"A": "secret://env/C4_RUNTIME_A", "B": "secret://env/C4_RUNTIME_B"}}

	_, _, err := ResolveContainerSpecSecretsWithIdentities(context.Background(), secret.ForNamespace(inner, "marketing").Resolver(), spec)
	require.ErrorIs(t, err, secret.ErrSecretDenied, "the second reference is checked against the reloaded rules")
	require.NotContains(t, err.Error(), "secret://env/C4_RUNTIME_")
}
