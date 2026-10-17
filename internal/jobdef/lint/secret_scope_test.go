package lint

import (
	"errors"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/pkg/container"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/require"
)

// These tests install the process-wide policy holder, so none is parallel.

func scopedLintDef(alias, namespace string, env map[string]string) schema.Definition {
	return schema.Definition{
		APIVersion: schema.APIVersionV1,
		Kind:       schema.KindJob,
		Metadata:   schema.Metadata{Alias: alias, Namespace: namespace},
		Trigger:    schema.Trigger{Configuration: map[string]any{"secret": "secret://env/LINT_HOOK"}},
		Steps:      []schema.Step{{Name: "run", Image: "alpine:3.23", Spec: container.Spec{Env: env}}},
	}
}

func installLintSecretPolicy(t *testing.T, rules map[string][]string) {
	t.Helper()
	view := policy.EmptyPolicy()
	for namespace, allow := range rules {
		view.Namespaces[namespace] = policy.Namespace{Secrets: &policy.SecretRules{Allow: allow}}
	}
	policy.SetProcessHolder(policy.NewEnvHolder(view, "", nil))
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
}

func countingResolver(calls *int, err error) func() (secret.Resolver, error) {
	return func() (secret.Resolver, error) {
		*calls++
		if err != nil {
			return nil, err
		}
		return &fakeResolver{}, nil
	}
}

func TestCheckSecretsAllowedWithoutRulesIsANoOp(t *testing.T) {
	policy.SetProcessHolder(nil)
	calls := 0
	defs := []schema.Definition{scopedLintDef("plain", "marketing", map[string]string{"A": "secret://env/ANYTHING"})}
	require.Empty(t, CheckSecretsAllowed(defs, countingResolver(&calls, nil)))
	require.Zero(t, calls, "no rules: no resolver is built")
}

func TestCheckSecretsAllowedReportsRedactedDenials(t *testing.T) {
	installLintSecretPolicy(t, map[string][]string{
		"c4-lint": {"env/LINT_*"},
		"default": {"env/LINT_*", "env/DEFAULT_*"},
	})
	calls := 0
	defs := []schema.Definition{
		scopedLintDef("allowed", "c4-lint", map[string]string{"A": "secret://env/LINT_TOKEN"}),
		scopedLintDef("denied", "c4-lint", map[string]string{
			"B": "secret://env/FINANCE_TOKEN",
			"A": "secret://env/LINT_TOKEN?name=PAYROLL_TOKEN",
			"C": "plain",
		}),
		// An omitted namespace is the default namespace, never "".
		scopedLintDef("omitted", "", map[string]string{"A": "secret://env/DEFAULT_TOKEN"}),
		scopedLintDef("omitted-denied", "", map[string]string{"A": "secret://env/OTHER_TOKEN"}),
		scopedLintDef("unruled", "c4-unruled", map[string]string{"A": "secret://env/ANYTHING"}),
	}
	msgs := CheckSecretsAllowed(defs, countingResolver(&calls, nil))
	require.Equal(t, 1, calls, "the resolver is built once, lazily")
	require.Equal(t, []string{
		`denied: secret env/FINANCE_TOKEN/… is not allowed by namespace "c4-lint" secret rules: no rule matches`,
		`denied: secret env/PAYROLL_TOKEN/… is not allowed by namespace "c4-lint" secret rules: no rule matches`,
		`omitted-denied: secret env/OTHER_TOKEN/… is not allowed by namespace "default" secret rules: no rule matches`,
	}, msgs)
	for _, msg := range msgs {
		require.False(t, strings.Contains(msg, "secret://"), msg)
	}
}

func TestCheckSecretsAllowedFailsClosedWithoutAResolver(t *testing.T) {
	installLintSecretPolicy(t, map[string][]string{"c4-lint": {"env/LINT_*"}})
	defs := []schema.Definition{scopedLintDef("job", "c4-lint", map[string]string{"A": "secret://env/LINT_TOKEN"})}
	calls := 0
	msgs := CheckSecretsAllowed(defs, countingResolver(&calls, errors.New("vault misconfigured")))
	require.Equal(t, []string{`job: secret rules of namespace "c4-lint" cannot be checked: vault misconfigured`}, msgs)
	require.Len(t, CheckSecretsAllowed(defs, nil), 1)
}
