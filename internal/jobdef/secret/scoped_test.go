package secret

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// scopedTestResolver is the production resolver chain (env + k8s + vault)
// with the Kubernetes secret resolver reading relative refs in kubeDefault.
func scopedTestResolver(t *testing.T, kubeDefault string) *MultiResolver {
	t.Helper()
	client := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "marketing-token", Namespace: "caesium-marketing", ResourceVersion: "7"},
		Data:       map[string][]byte{"token": []byte("k8s-value")},
	})
	return NewMultiResolver(map[string]Resolver{
		providerEnv:        NewEnvResolver(),
		providerKubernetes: NewKubernetesResolverWithClient(client, kubeDefault),
		"kubernetes":       NewKubernetesResolverWithClient(client, kubeDefault),
		providerVault:      NewVaultResolverWithLogical(&fakeLogical{}),
	})
}

func TestCanonicalTargetUsesEachProvidersEffectiveTarget(t *testing.T) {
	t.Parallel()
	resolver := scopedTestResolver(t, "secrets-ns")
	for _, tc := range []struct {
		ref, want string
	}{
		// env: ?name= wins over the path; path segments join with _.
		{"secret://env/MARKETING_TOKEN", "env/MARKETING_TOKEN"},
		{"secret://env/MARKETING/TOKEN", "env/MARKETING_TOKEN"},
		{"secret://env/MARKETING_TOKEN?name=FINANCE_TOKEN", "env/FINANCE_TOKEN"},
		{"secret://env/ignored?name=%20FINANCE_TOKEN%20", "env/FINANCE_TOKEN"},
		{"secret://ENV/MARKETING_TOKEN", "env/MARKETING_TOKEN"},
		{"secret://env/x?name=MARKETING_A/../FINANCE", "env/MARKETING_A/../FINANCE"},
		// vault: ?field= wins, else the last segment is the field; the URL
		// fragment is never read; %2F splits like the read does.
		{"secret://vault/secret/data/marketing/token/password", "vault/secret/data/marketing/token#password"},
		{"secret://vault/secret/data/marketing/token?field=password", "vault/secret/data/marketing/token#password"},
		{"secret://vault/secret/data/marketing?field=token", "vault/secret/data/marketing#token"},
		{"secret://vault/secret/data/marketing/token#password", "vault/secret/data/marketing#token"},
		{"secret://vault/secret%2Fdata/finance/token/password", "vault/secret/data/finance/token#password"},
		{"secret://vault/secret/data/marketing/token?field=a/b", "vault/secret/data/marketing/token#a/b"},
		// k8s: relative refs read in the resolver's configured namespace; the
		// path namespace and every query override are honoured; the key is
		// not part of the target; kubernetes is an alias.
		{"secret://k8s/marketing-token/token", "k8s/secrets-ns/marketing-token"},
		{"secret://k8s/finance/marketing-token/token", "k8s/finance/marketing-token"},
		{"secret://k8s/marketing-token/token?namespace=finance", "k8s/finance/marketing-token"},
		{"secret://k8s/caesium-marketing/marketing-token/token?namespace=finance", "k8s/finance/marketing-token"},
		{"secret://k8s/marketing-token/token?name=finance-token", "k8s/secrets-ns/finance-token"},
		{"secret://k8s/marketing-token/token?key=other", "k8s/secrets-ns/marketing-token"},
		{"secret://kubernetes/marketing-token/token", "k8s/secrets-ns/marketing-token"},
		{"secret://k8s/marketing-token/token?namespace=..", "k8s/../marketing-token"},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			t.Parallel()
			got, err := resolver.CanonicalTarget(tc.ref)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestCanonicalTargetRejectsUncheckableReferences(t *testing.T) {
	t.Parallel()
	resolver := scopedTestResolver(t, "secrets-ns")
	for _, ref := range []string{
		"secret://vault/secret/data/marketing/token%23x/password",
		"secret://vault/secret/data/marketing/token/pass%23word",
		"secret://vault/secret/data/marketing/token?field=a%23b",
		"secret://vault/secret/data/marketing/token?field=a%23b%23c",
		"secret://vault/onlypath",
		"secret://k8s/marketing-token/token?name=a/b",
		"secret://k8s/marketing-token/token?namespace=a/b",
		"secret://k8s/marketing-token/token?namespace=../x",
		"secret://k8s/only-one",
		"secret://env/",
		"secret://aws/token",
		"https://env/TOKEN",
		"secret:env/TOKEN",
		"::",
	} {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()
			_, err := resolver.CanonicalTarget(ref)
			require.Error(t, err)
		})
	}
	unconfigured := NewMultiResolver(map[string]Resolver{providerEnv: NewEnvResolver()})
	_, err := unconfigured.CanonicalTarget("secret://vault/secret/data/x/token/password")
	require.ErrorContains(t, err, `secret provider "vault" not configured`)
}

func marketingRules(allow ...string) *policy.SecretRules {
	return &policy.SecretRules{Allow: allow}
}

func TestScopedResolverAllowMatchesCanonicalTargets(t *testing.T) {
	t.Parallel()
	rules := marketingRules(
		"env/MARKETING_*",
		"vault/secret/data/marketing/**",
		"k8s/marketing-*",          // relative: the mapped namespace only
		"k8s/shared/common-*",      // absolute
		"kubernetes/aliased/alias", // absolute, alias spelling
	)
	// The secret resolver reads relative refs in "default" while the mapped
	// namespace is caesium-marketing: the two settings differ, so a relative
	// ref never matches a relative rule (fail closed; C1 aligns them).
	scoped := NewScopedResolver(scopedTestResolver(t, "default"), "marketing", rules, "caesium-marketing")
	for _, tc := range []struct {
		ref   string
		allow bool
	}{
		{"secret://env/MARKETING_TOKEN", true},
		{"secret://env/FINANCE_TOKEN", false},
		{"secret://env/MARKETING_TOKEN?name=FINANCE_TOKEN", false},
		{"secret://env/FINANCE_TOKEN?name=MARKETING_TOKEN", true},
		{"secret://env/MARKETING/TOKEN", true},
		{"secret://env/x?name=MARKETING_A/../FINANCE", false},
		{"secret://env/x?name=MARKETING_A/FINANCE", false},
		{"secret://vault/secret/data/marketing/token/password", true},
		{"secret://vault/secret/data/marketing/token?field=password", true},
		{"secret://vault/secret/data/marketing?field=token", true},
		{"secret://vault/secret/data/marketing/token?field=a/b", true},
		{"secret://vault/secret/data/finance/token/password", false},
		{"secret://vault/secret/data/marketing/../finance/token/password", false},
		{"secret://vault/secret%2Fdata/finance/token/password", false},
		{"secret://vault/secret/data/marketing/token%23x/password", false},
		{"secret://vault/secret/data/marketing/token?field=a%23b", false},
		{"secret://vault/secret/data//marketing/token/password", false},
		{"secret://k8s/caesium-marketing/marketing-token/token", true},
		{"secret://k8s/marketing-token/token?namespace=caesium-marketing", true},
		{"secret://k8s/marketing-token/token", false},
		{"secret://k8s/caesium-marketing/marketing-token/token?namespace=finance", false},
		{"secret://k8s/caesium-marketing/marketing-token/token?name=finance-token", false},
		{"secret://k8s/caesium-marketing/marketing-token/token?namespace=../caesium-marketing", false},
		{"secret://k8s/caesium-marketing/marketing-token/token?namespace=..", false},
		{"secret://k8s/shared/common-a/token", true},
		{"secret://k8s/other/common-a/token", false},
		{"secret://kubernetes/aliased/alias/token", true},
		{"secret://k8s/aliased/alias/token", true},
		{"secret://aws/token", false},
		{"not a reference", false},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			t.Parallel()
			err := scoped.Allow(tc.ref)
			if tc.allow {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrSecretDenied)
			require.Contains(t, err.Error(), DeniedMarker)
			require.Contains(t, err.Error(), `namespace "marketing"`)
		})
	}
}

func TestScopedResolverRelativeRulesFollowTheMappedNamespace(t *testing.T) {
	t.Parallel()
	// Both settings agree: a relative ref and a relative rule meet.
	aligned := NewScopedResolver(scopedTestResolver(t, "caesium-marketing"), "marketing", marketingRules("k8s/**"), "caesium-marketing")
	require.NoError(t, aligned.Allow("secret://k8s/marketing-token/token"))
	require.NoError(t, aligned.Allow("secret://k8s/caesium-marketing/any/token"))
	// k8s/** is relative, so it is pinned to the mapped namespace.
	require.ErrorIs(t, aligned.Allow("secret://k8s/default/any/token"), ErrSecretDenied)
	require.ErrorIs(t, aligned.Allow("secret://k8s/marketing-token/token?namespace=finance"), ErrSecretDenied)

	// The engine default (CAESIUM_KUBERNETES_NAMESPACE) differs from the
	// secret resolver default: the rule grants only the engine namespace.
	split := NewScopedResolver(scopedTestResolver(t, "secrets-ns"), "marketing", marketingRules("k8s/marketing-*"), "caesium")
	require.ErrorIs(t, split.Allow("secret://k8s/marketing-token/token"), ErrSecretDenied)
	require.NoError(t, split.Allow("secret://k8s/caesium/marketing-token/token"))

	// A mapped namespace that is not a DNS label never widens a relative rule:
	// the rule is dropped, so it grants nothing.
	for _, bad := range []string{"", "*", "a/b", "Caesium", "**"} {
		dropped := NewScopedResolver(scopedTestResolver(t, "x"), "marketing", marketingRules("k8s/*", "k8s/**"), bad)
		require.ErrorIs(t, dropped.Allow("secret://k8s/x/secret/token"), ErrSecretDenied, bad)
		require.ErrorIs(t, dropped.Allow("secret://k8s/secret/token"), ErrSecretDenied, bad)
	}
}

func TestScopedResolverEmptyRulesDenyAndNilRulesAllowUnchanged(t *testing.T) {
	t.Parallel()
	resolver := scopedTestResolver(t, "default")

	denyAll := NewScopedResolver(resolver, "marketing", marketingRules(), "caesium")
	require.False(t, denyAll.AllowsAll())
	require.ErrorIs(t, denyAll.Allow("secret://env/PATH"), ErrSecretDenied)
	_, err := denyAll.Resolve(context.Background(), "secret://env/PATH")
	require.ErrorIs(t, err, ErrSecretDenied)

	allowAll := NewScopedResolver(resolver, "marketing", nil, "caesium")
	require.True(t, allowAll.AllowsAll())
	require.NoError(t, allowAll.Allow("secret://aws/not-even-a-provider"))
	// Allow-all resolution is the inner resolver's, error text included.
	_, innerErr := resolver.Resolve(context.Background(), "secret://env/CAESIUM_SCOPED_TEST_UNSET")
	_, scopedErr := allowAll.Resolve(context.Background(), "secret://env/CAESIUM_SCOPED_TEST_UNSET")
	require.Error(t, innerErr)
	require.Equal(t, innerErr.Error(), scopedErr.Error())
	value, err := allowAll.Resolve(context.Background(), "secret://k8s/caesium-marketing/marketing-token/token")
	require.NoError(t, err)
	require.Equal(t, "k8s-value", value)
}

func TestScopedResolverRedactsDenials(t *testing.T) {
	t.Parallel()
	scoped := NewScopedResolver(scopedTestResolver(t, "default"), "marketing", marketingRules("env/MARKETING_*"), "caesium")
	for _, tc := range []struct {
		ref, redacted, never string
	}{
		{"secret://vault/secret/data/finance/token/password", "vault/secret/…", "finance"},
		{"secret://k8s/marketing-token/token?namespace=finance", "k8s/finance/…", "marketing-token"},
		{"secret://kubernetes/finance/payroll/token", "k8s/finance/…", "payroll"},
		{"secret://env/FINANCE_TOKEN", "env/FINANCE_TOKEN/…", "secret://"},
		{"secret://vault/secret/data/finance%23x/token/password", "vault/…", "finance"},
		{"secret://vault/secret/data/finance/token?field=a%23b", "vault/…", "finance"},
		{"secret://aws/finance/token", "secret/…", "finance"},
		{"::finance", "secret/…", "finance"},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			t.Parallel()
			err := scoped.Allow(tc.ref)
			var denied *SecretDeniedError
			require.ErrorAs(t, err, &denied)
			require.Equal(t, tc.redacted, denied.Ref)
			require.Equal(t, "marketing", denied.Namespace)
			require.Contains(t, err.Error(), "secret "+tc.redacted+" is not allowed by namespace")
			require.NotContains(t, err.Error(), tc.never)
			require.NotContains(t, err.Error(), tc.ref)
		})
	}
}

// verifyingResolver is an inner resolver that also verifies identities.
type verifyingResolver struct {
	*MultiResolver
	verified []string
}

func (v *verifyingResolver) VerifyIdentity(_ context.Context, ref string, expected Identity) (Identity, error) {
	v.verified = append(v.verified, ref)
	return expected, nil
}

func (v *verifyingResolver) VerifyResolvedIdentity(_ context.Context, ref string, expected Identity, _ string) (Identity, error) {
	v.verified = append(v.verified, ref)
	return expected, nil
}

func TestScopedResolverChecksBeforeVerification(t *testing.T) {
	t.Parallel()
	inner := &verifyingResolver{MultiResolver: scopedTestResolver(t, "default")}
	scoped := NewScopedResolver(inner, "marketing", marketingRules("env/MARKETING_*"), "caesium")

	_, err := scoped.VerifyIdentity(context.Background(), "secret://env/FINANCE", Identity{Provider: "env"})
	require.ErrorIs(t, err, ErrSecretDenied)
	_, err = scoped.VerifyResolvedIdentity(context.Background(), "secret://env/FINANCE", Identity{Provider: "env"}, "v")
	require.ErrorIs(t, err, ErrSecretDenied)
	require.Empty(t, inner.verified, "a denied reference never reaches the inner verifier")

	_, err = scoped.VerifyIdentity(context.Background(), "secret://env/MARKETING_A", Identity{Provider: "env"})
	require.NoError(t, err)
	_, err = scoped.VerifyResolvedIdentity(context.Background(), "secret://env/MARKETING_B", Identity{Provider: "env"}, "v")
	require.NoError(t, err)
	require.Equal(t, []string{"secret://env/MARKETING_A", "secret://env/MARKETING_B"}, inner.verified)

	// An inner resolver without verifiers keeps MultiResolver's wording.
	plain := NewScopedResolver(NewEnvResolver(), "marketing", marketingRules("env/MARKETING_*"), "caesium")
	_, err = plain.VerifyIdentity(context.Background(), "secret://env/MARKETING_A", Identity{Provider: "env"})
	require.EqualError(t, err, `secret provider "env" does not support baseline identity verification`)
	_, err = plain.VerifyResolvedIdentity(context.Background(), "secret://env/MARKETING_A", Identity{Provider: "env"}, "v")
	require.EqualError(t, err, `secret provider "env" does not support resolved baseline identity verification`)
}

func TestScopedResolverWithoutInner(t *testing.T) {
	t.Parallel()
	allowAll := NewScopedResolver(nil, "", nil, "")
	require.Nil(t, allowAll.Resolver(), "no inner resolver keeps callers' nil handling")
	require.Equal(t, "default", allowAll.Namespace())
	require.NoError(t, allowAll.Allow("secret://env/PATH"))

	ruled := NewScopedResolver(nil, "marketing", marketingRules("env/**"), "")
	require.ErrorIs(t, ruled.Allow("secret://env/PATH"), ErrSecretDenied)
	_, err := ruled.CanonicalTarget("secret://env/PATH")
	require.Error(t, err)

	var none *ScopedResolver
	require.ErrorIs(t, none.Allow("secret://env/PATH"), ErrSecretDenied)
	require.False(t, none.AllowsAll())
	require.Nil(t, none.Resolver())

	scoped := NewScopedResolver(NewEnvResolver(), "marketing", nil, "")
	require.Same(t, scoped, scoped.Resolver())
}

func writeScopedPolicy(t *testing.T, body string) *policy.Holder {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	holder, err := policy.LoadFile(context.Background(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	return holder
}

const scopedPolicy = `apiVersion: v1
kind: AccessPolicy
namespaces:
  finance: {}
  marketing:
    kubernetes: {namespace: caesium-marketing}
    secrets: {allow: ["env/MARKETING_*", "k8s/marketing-*"]}
  locked:
    secrets: {allow: []}
  unmapped:
    secrets: {allow: ["k8s/shared-*"]}
`

func TestForNamespaceReadsTheHolder(t *testing.T) {
	t.Parallel()
	inner := scopedTestResolver(t, "caesium-marketing")
	file := writeScopedPolicy(t, scopedPolicy)

	// default and a declared namespace without rules: allow all.
	for _, ns := range []string{"", "default", "finance"} {
		scoped := forNamespace(inner, ns, file, "caesium", nil)
		require.True(t, scoped.AllowsAll(), ns)
		require.NoError(t, scoped.Allow("secret://env/ANYTHING"), ns)
	}
	require.Equal(t, "default", forNamespace(inner, "", file, "caesium", nil).Namespace())

	// Rules: enforced, with relative k8s rules on the policy's mapping.
	marketing := forNamespace(inner, "marketing", file, "caesium", nil)
	require.False(t, marketing.AllowsAll())
	require.NoError(t, marketing.Allow("secret://env/MARKETING_TOKEN"))
	require.NoError(t, marketing.Allow("secret://k8s/marketing-token/token"))
	require.ErrorIs(t, marketing.Allow("secret://env/FINANCE_TOKEN"), ErrSecretDenied)

	// No mapping: relative rules mean CAESIUM_KUBERNETES_NAMESPACE.
	unmapped := forNamespace(inner, "unmapped", file, "caesium", nil)
	require.NoError(t, unmapped.Allow("secret://k8s/caesium/shared-db/password"))
	require.ErrorIs(t, unmapped.Allow("secret://k8s/caesium-marketing/shared-db/password"), ErrSecretDenied)
	require.NoError(t, forNamespace(inner, "unmapped", file, "", nil).Allow("secret://k8s/default/shared-db/password"),
		"an unset engine namespace falls back to default, like the engine")

	require.ErrorIs(t, forNamespace(inner, "locked", file, "caesium", nil).Allow("secret://env/MARKETING_TOKEN"), ErrSecretDenied)

	// Undeclared on a node serving a policy file: deny all, named as such.
	err := forNamespace(inner, "ghost", file, "caesium", nil).Allow("secret://env/MARKETING_TOKEN")
	require.ErrorIs(t, err, ErrSecretDenied)
	require.ErrorContains(t, err, "not declared in this node's access policy")

	// No policy file (nil or env-translated holder): allow all everywhere.
	for _, holder := range []*policy.Holder{nil, policy.NewEnvHolder(nil, "", nil)} {
		for _, ns := range []string{"", "default", "ghost", "marketing"} {
			require.True(t, forNamespace(inner, ns, holder, "caesium", nil).AllowsAll(), ns)
		}
	}
}

// TestForNamespaceAndScopingActiveUseTheProcessHolder is not parallel: it
// installs the process-wide holder.
func TestForNamespaceAndScopingActiveUseTheProcessHolder(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	inner := scopedTestResolver(t, "default")

	policy.SetProcessHolder(nil)
	require.False(t, ScopingActive())
	require.True(t, ForNamespace(inner, "marketing").AllowsAll())

	policy.SetProcessHolder(policy.NewEnvHolder(nil, "", nil))
	require.False(t, ScopingActive())

	view := policy.EmptyPolicy()
	view.Namespaces["marketing"] = policy.Namespace{Secrets: &policy.SecretRules{Allow: []string{"env/M_*"}}}
	policy.SetProcessHolder(policy.NewEnvHolder(view, "", nil))
	require.True(t, ScopingActive(), "a declared namespace with rules activates scoping")
	require.ErrorIs(t, ForNamespace(inner, "marketing").Allow("secret://env/PATH"), ErrSecretDenied)

	policy.SetProcessHolder(writeScopedPolicy(t, "apiVersion: v1\nkind: AccessPolicy\n"))
	require.True(t, ScopingActive(), "a policy file activates scoping")
	require.True(t, ForNamespace(inner, "default").AllowsAll())
	require.ErrorIs(t, ForNamespace(inner, "ghost").Allow("secret://env/PATH"), ErrSecretDenied)
}

// TestPendingScopeWaitsForTheConfiguredPolicy covers work launched during
// startup, before cmd/start installs the holder of a configured policy file:
// the scope never allows on the empty policy; it waits for the holder on use
// and denies when it never appears. Not parallel: it installs the process
// holder and shortens the wait.
func TestPendingScopeWaitsForTheConfiguredPolicy(t *testing.T) {
	original := processHolderWait
	t.Cleanup(func() {
		processHolderWait = original
		policy.SetProcessHolder(nil)
	})
	inner := scopedTestResolver(t, "default")
	policy.SetProcessHolder(nil)

	// No policy file configured: a nil holder is the empty policy.
	require.True(t, forProcess(inner, "marketing", nil, false, "caesium", nil).AllowsAll())

	// Configured but never installed: denied after the wait, never allowed.
	processHolderWait = 100 * time.Millisecond
	pending := forProcess(inner, "default", nil, true, "caesium", nil)
	require.False(t, pending.AllowsAll())
	err := pending.Allow("secret://env/MARKETING_TOKEN")
	require.ErrorIs(t, err, ErrSecretDenied)
	require.ErrorContains(t, err, "the configured access policy is not loaded")
	require.ErrorContains(t, err, "env/MARKETING_TOKEN/…")

	// Installed while waiting: the installed policy decides.
	processHolderWait = 10 * time.Second
	file := writeScopedPolicy(t, scopedPolicy)
	pending = forProcess(inner, "marketing", nil, true, "caesium", nil)
	go func() {
		time.Sleep(50 * time.Millisecond)
		policy.SetProcessHolder(file)
	}()
	require.NoError(t, pending.Allow("secret://env/MARKETING_TOKEN"))
	require.ErrorIs(t, pending.Allow("secret://env/FINANCE_TOKEN"), ErrSecretDenied)
	_, err = pending.Resolve(context.Background(), "secret://env/FINANCE_TOKEN")
	require.ErrorIs(t, err, ErrSecretDenied)

	// A file-configured process with the holder installed is not pending.
	require.False(t, forProcess(inner, "marketing", file, true, "caesium", nil).pending)
}

// TestClientScopeNeverWaitsForAServerPolicy pins the client-side exemption: a
// client process (`caesium dev`, `reproduce`, a no-server `run retry`) never
// installs the server's policy holder, so a CAESIUM_AUTH_POLICY_FILE inherited
// from its environment must not make its scope pending, while the server's
// ForNamespace stays pending in the same process state. A holder the process
// did install still applies. Not parallel: it sets the process environment and
// the holder.
func TestClientScopeNeverWaitsForAServerPolicy(t *testing.T) {
	original := processHolderWait
	t.Cleanup(func() {
		processHolderWait = original
		policy.SetProcessHolder(nil)
		require.NoError(t, env.Process())
	})
	t.Setenv("CAESIUM_AUTH_POLICY_FILE", filepath.Join(t.TempDir(), "policy.yaml"))
	t.Setenv("CAESIUM_AUTH_ROLE_MAPPING", "")
	t.Setenv("MARKETING_TOKEN", "client-value")
	require.NoError(t, env.Process())
	policy.SetProcessHolder(nil)
	processHolderWait = 10 * time.Second
	inner := scopedTestResolver(t, "default")

	require.True(t, ForNamespace(inner, "marketing").pending, "the server waits for its configured policy")

	client := ForClientNamespace(inner, "marketing")
	require.False(t, client.pending)
	require.True(t, client.AllowsAll())
	started := time.Now()
	value, err := client.Resolve(context.Background(), "secret://env/MARKETING_TOKEN")
	require.NoError(t, err)
	require.Equal(t, "client-value", value)
	require.NoError(t, client.Allow("secret://env/FINANCE_TOKEN"))
	require.Less(t, time.Since(started), time.Second, "a client never waits for the server's policy")

	policy.SetProcessHolder(writeScopedPolicy(t, scopedPolicy))
	installed := ForClientNamespace(inner, "marketing")
	require.NoError(t, installed.Allow("secret://env/MARKETING_TOKEN"))
	require.ErrorIs(t, installed.Allow("secret://env/FINANCE_TOKEN"), ErrSecretDenied)
}

// revokeOnReadResolver revokes once, while its first reference is being read.
type revokeOnReadResolver struct {
	Resolver
	once   sync.Once
	revoke func()
}

func (r *revokeOnReadResolver) ResolveWithIdentity(ctx context.Context, ref string) (string, Identity, error) {
	r.once.Do(r.revoke)
	return r.Resolver.ResolveWithIdentity(ctx, ref)
}

// TestProcessScopeReadsThePolicyForEveryReference: a process-backed scope
// re-reads the policy on every check, so a reload while one reference is being
// read denies the next reference of the same resolution. Not parallel: it
// installs the process holder.
func TestProcessScopeReadsThePolicyForEveryReference(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	t.Setenv("MARKETING_TOKEN", "first")
	t.Setenv("MARKETING_OTHER", "second")
	rules := func(allow ...string) *policy.Holder {
		view := policy.EmptyPolicy()
		view.Namespaces["marketing"] = policy.Namespace{Secrets: &policy.SecretRules{Allow: append([]string{}, allow...)}}
		return policy.NewEnvHolder(view, "", nil)
	}
	policy.SetProcessHolder(rules("env/MARKETING_*"))
	inner := &revokeOnReadResolver{Resolver: scopedTestResolver(t, "default"), revoke: func() { policy.SetProcessHolder(rules()) }}
	scope := ForNamespace(inner, "marketing")

	value, err := scope.Resolve(context.Background(), "secret://env/MARKETING_TOKEN")
	require.NoError(t, err, "the read already in flight completes")
	require.Equal(t, "first", value)
	_, err = scope.Resolve(context.Background(), "secret://env/MARKETING_OTHER")
	require.ErrorIs(t, err, ErrSecretDenied, "the next reference reads the reloaded policy")
	require.ErrorIs(t, scope.Allow("secret://env/MARKETING_TOKEN"), ErrSecretDenied)

	policy.SetProcessHolder(rules("env/MARKETING_*"))
	require.NoError(t, scope.Allow("secret://env/MARKETING_TOKEN"), "a scope follows reloads both ways")
	policy.SetProcessHolder(nil)
	require.NoError(t, ForClientNamespace(inner, "marketing").Allow("secret://env/FINANCE_TOKEN"))
}

// TestSecretRuleProvidersMatchTheResolverChain pins the policy package's
// supported-provider list to the providers NewConfiguredResolver registers.
func TestSecretRuleProvidersMatchTheResolverChain(t *testing.T) {
	t.Parallel()
	resolver, err := NewConfiguredResolver(Config{
		EnableEnv:  true,
		Kubernetes: &KubernetesConfig{Namespace: "default"},
		Vault:      &VaultConfig{Address: "http://127.0.0.1:1"},
	})
	require.NoError(t, err)
	require.Equal(t, policy.SecretProviders(), resolver.Providers())
	for _, provider := range resolver.Providers() {
		require.NotEmpty(t, policy.CanonicalSecretProvider(provider), provider)
	}
}

func TestSecretDeniedErrorShape(t *testing.T) {
	t.Parallel()
	err := error(&SecretDeniedError{Namespace: "marketing", Ref: "env/PATH/…", Reason: "no rule matches"})
	require.EqualError(t, err, `secret env/PATH/… is not allowed by namespace "marketing" secret rules: no rule matches`)
	require.ErrorIs(t, err, ErrSecretDenied)
	require.True(t, strings.Contains(err.Error(), DeniedMarker))
	require.False(t, errors.Is(errors.New("secret env/PATH denied"), ErrSecretDenied))
}
