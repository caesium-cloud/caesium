package secret

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// mappingTestResolver serves a "token" Secret, with a distinct value, in the
// resolver's default namespace and in two mapped namespaces.
func mappingTestResolver(t *testing.T) *MultiResolver {
	t.Helper()
	secretIn := func(namespace string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "token", Namespace: namespace, ResourceVersion: "1"},
			Data:       map[string][]byte{"value": []byte("from-" + namespace)},
		}
	}
	client := fake.NewClientset(secretIn("default"), secretIn("caesium-marketing"), secretIn("caesium-marketing-v2"))
	kube := NewKubernetesResolverWithClient(client, "default")
	return NewMultiResolver(map[string]Resolver{
		providerEnv:        NewEnvResolver(),
		providerKubernetes: kube,
		"kubernetes":       kube,
	})
}

const mappingPolicyA = `apiVersion: v1
kind: AccessPolicy
namespaces:
  marketing:
    kubernetes: {namespace: caesium-marketing}
    secrets: {allow: ["k8s/token"]}
  open:
    kubernetes: {namespace: caesium-marketing}
  plain:
    secrets: {allow: ["k8s/**", "k8s/default/token"]}
`

const mappingPolicyB = `apiVersion: v1
kind: AccessPolicy
namespaces:
  marketing:
    kubernetes: {namespace: caesium-marketing-v2}
    secrets: {allow: ["k8s/token"]}
`

func TestRelativeKubernetesReferencesReadInTheMappedNamespace(t *testing.T) {
	t.Parallel()
	inner := mappingTestResolver(t)
	holder := writeScopedPolicy(t, mappingPolicyA)
	ctx := context.Background()

	for _, scope := range []*ScopedResolver{
		forNamespace(inner, "marketing", holder, "caesium", nil),
		forNamespace(inner, "open", holder, "caesium", nil), // no rules: allow all, still mapped
	} {
		for _, ref := range []string{"secret://k8s/token/value", "secret://kubernetes/token/value"} {
			value, identity, err := scope.ResolveWithIdentity(ctx, ref)
			require.NoError(t, err, ref)
			require.Equal(t, "from-caesium-marketing", value, ref)
			require.Equal(t, "caesium-marketing", identity.Namespace, ref)
			target, err := scope.CanonicalTarget(ref)
			require.NoError(t, err)
			require.Equal(t, "k8s/caesium-marketing/token", target)
		}
		// A reference naming its own namespace keeps it.
		value, err := scope.Resolve(ctx, "secret://k8s/token/value?namespace=caesium-marketing-v2")
		if scope.AllowsAll() {
			require.NoError(t, err)
			require.Equal(t, "from-caesium-marketing-v2", value)
		} else {
			require.ErrorIs(t, err, ErrSecretDenied, "the relative rule grants only the mapped namespace")
		}
	}

	// A namespace without a mapping keeps the resolver's default namespace.
	plain := forNamespace(inner, "plain", holder, "caesium", nil)
	value, err := plain.Resolve(ctx, "secret://k8s/default/token/value")
	require.NoError(t, err)
	require.Equal(t, "from-default", value)
	target, err := plain.CanonicalTarget("secret://k8s/token/value")
	require.NoError(t, err)
	require.Equal(t, "k8s/default/token", target, "no mapping: CAESIUM_JOBDEF_SECRETS_KUBE_NAMESPACE, unchanged")

	// The inner resolver itself is never mutated.
	direct, err := inner.Resolve(ctx, "secret://k8s/token/value")
	require.NoError(t, err)
	require.Equal(t, "from-default", direct)
}

func TestPinnedMappingHoldsForTheExecution(t *testing.T) {
	t.Parallel()
	inner := mappingTestResolver(t)
	holder := writeScopedPolicy(t, mappingPolicyB)
	pinned := "caesium-marketing"
	// The execution resolved caesium-marketing; the live policy now maps v2.
	// The read stays in the pinned namespace, and the live relative rule (now
	// k8s/caesium-marketing-v2/token) no longer matches it: denied, not widened.
	scope := forNamespace(inner, "marketing", holder, "caesium", &pinned)
	_, err := scope.Resolve(context.Background(), "secret://k8s/token/value")
	require.ErrorIs(t, err, ErrSecretDenied)

	empty := ""
	unpinned := forNamespace(inner, "marketing", holder, "caesium", &empty)
	target, err := unpinned.CanonicalTarget("secret://k8s/token/value")
	require.NoError(t, err)
	require.Equal(t, "k8s/default/token", target, "an empty pin is the resolver default")
}

// TestProcessScopeRefreshesAllowAndReadTogether builds a process scope under
// policy A, reloads to B, and resolves a relative reference: the allow-check
// and the read both come from B, so the reference is allowed by B's rule and
// read in B's mapped namespace.
func TestProcessScopeRefreshesAllowAndReadTogether(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	inner := mappingTestResolver(t)
	policy.SetProcessHolder(writeScopedPolicy(t, mappingPolicyA))
	scope := ForNamespace(inner, "marketing")
	value, err := scope.Resolve(context.Background(), "secret://k8s/token/value")
	require.NoError(t, err)
	require.Equal(t, "from-caesium-marketing", value)

	policy.SetProcessHolder(writeScopedPolicy(t, mappingPolicyB))
	value, identity, err := scope.ResolveWithIdentity(context.Background(), "secret://kubernetes/token/value")
	require.NoError(t, err)
	require.Equal(t, "from-caesium-marketing-v2", value)
	require.Equal(t, "caesium-marketing-v2", identity.Namespace)
	target, err := scope.CanonicalTarget("secret://k8s/token/value")
	require.NoError(t, err)
	require.Equal(t, "k8s/caesium-marketing-v2/token", target)

	// The execution-pinned scope keeps its namespace across the same reload.
	policy.SetProcessHolder(writeScopedPolicy(t, mappingPolicyA))
	pinned := ForNamespaceTarget(inner, "marketing", "caesium-marketing")
	value, err = pinned.Resolve(context.Background(), "secret://k8s/token/value")
	require.NoError(t, err)
	require.Equal(t, "from-caesium-marketing", value)
	policy.SetProcessHolder(writeScopedPolicy(t, mappingPolicyB))
	_, err = pinned.Resolve(context.Background(), "secret://k8s/token/value")
	require.ErrorIs(t, err, ErrSecretDenied, "a remap mid-execution never widens the pinned read")
}

func TestRelativeKubernetesReference(t *testing.T) {
	t.Parallel()
	for env, want := range map[string]bool{
		"secret://k8s/token/value":                     true,
		"secret://kubernetes/token/value":              true,
		"secret://k8s/token/value?key=other":           true,
		"secret://k8s/ns/token/value":                  false,
		"secret://k8s/token/value?namespace=elsewhere": false,
		"secret://env/MARKETING_TOKEN":                 false,
		"plain-value":                                  false,
		"secret://k8s":                                 false,
	} {
		require.Equal(t, want, RelativeKubernetesReference(map[string]string{"A": "x", "TOKEN": env}), env)
	}
	require.False(t, RelativeKubernetesReference(nil))
}

func TestWithKubernetesNamespaceLeavesOtherResolversAlone(t *testing.T) {
	t.Parallel()
	env := NewEnvResolver()
	require.Same(t, Resolver(env), withKubernetesNamespace(env, "caesium-marketing"))
	multi := NewMultiResolver(map[string]Resolver{providerEnv: env})
	require.Same(t, Resolver(multi), withKubernetesNamespace(multi, "caesium-marketing"), "no Kubernetes provider: unchanged")
	kube := NewKubernetesResolverWithClient(fake.NewClientset(), "default")
	require.Same(t, Resolver(kube), withKubernetesNamespace(kube, ""))
	mapped, ok := withKubernetesNamespace(kube, "caesium-marketing").(*mappedKubernetesResolver)
	require.True(t, ok)
	require.Equal(t, "caesium-marketing", mapped.namespace)
}

// TestServerPolicy: the installed holder; the empty policy without a file;
// and, with a configured file that never loads, not ok after the wait. Not
// parallel: it sets the process environment, the holder and the wait.
func TestServerPolicy(t *testing.T) {
	original := processHolderWait
	t.Cleanup(func() {
		processHolderWait = original
		policy.SetProcessHolder(nil)
		_ = env.Process()
	})
	policy.SetProcessHolder(nil)
	t.Setenv("CAESIUM_AUTH_POLICY_FILE", "")
	require.NoError(t, env.Process())
	holder, ok := ServerPolicy()
	require.True(t, ok)
	require.Nil(t, holder)

	t.Setenv("CAESIUM_AUTH_POLICY_FILE", "/nonexistent/policy.yaml")
	require.NoError(t, env.Process())
	processHolderWait = 50 * time.Millisecond
	_, ok = ServerPolicy()
	require.False(t, ok, "a configured policy that never loads is not the empty policy")

	installed := writeScopedPolicy(t, mappingPolicyA)
	policy.SetProcessHolder(installed)
	holder, ok = ServerPolicy()
	require.True(t, ok)
	require.Same(t, installed, holder)
}
