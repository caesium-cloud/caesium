package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/incident"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes/fake"
)

const isolationPolicy = `apiVersion: v1
kind: AccessPolicy
namespaces:
  marketing:
    kubernetes: {namespace: caesium-marketing, serviceAccountName: marketing-jobs}
    secrets: {allow: ["env/MARKETING_*", "k8s/marketing-*"]}
  accounts:
    kubernetes: {serviceAccountName: shared-jobs}
  locked:
    secrets: {allow: []}
  finance: {}
`

func loadIsolationPolicy(t *testing.T, body string) *policy.Holder {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	holder, err := policy.LoadFile(context.Background(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	return holder
}

func setIsolationEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	// Registered before Setenv so it runs after the variables are restored.
	t.Cleanup(func() { _ = env.Process() })
	for k, v := range kv {
		t.Setenv(k, v)
	}
	require.NoError(t, env.Process())
}

// Not parallel: installs the process holder and processes the environment.
func TestResolveIsolationContext(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	setIsolationEnv(t, map[string]string{
		"CAESIUM_AUTH_POLICY_FILE":              "",
		"CAESIUM_KUBERNETES_NAMESPACE":          "owner-pods",
		"CAESIUM_JOBDEF_SECRETS_KUBE_NAMESPACE": "owner-secrets",
	})

	// No policy file: nothing is carried and dispatch is unchanged.
	policy.SetProcessHolder(nil)
	_, carry, ready := ResolveIsolationContext("marketing")
	require.False(t, carry)
	require.True(t, ready)
	policy.SetProcessHolder(policy.NewEnvHolder(nil, "", nil))
	_, carry, ready = ResolveIsolationContext("marketing")
	require.False(t, carry, "an env-translated policy carries nothing")
	require.True(t, ready)

	holder := loadIsolationPolicy(t, isolationPolicy)
	policy.SetProcessHolder(holder)
	revision := holder.Current().Hash
	require.NotEmpty(t, revision)

	got, carry, ready := ResolveIsolationContext("marketing")
	require.True(t, carry)
	require.True(t, ready)
	require.False(t, got.Default())
	require.Equal(t, IsolationContext{
		Namespace:                  "marketing",
		PolicyRevision:             revision,
		Kubernetes:                 &atom.KubeTarget{Namespace: "caesium-marketing", ServiceAccountName: "marketing-jobs"},
		DefaultKubernetesNamespace: "owner-pods",
		SecretReadNamespace:        "caesium-marketing",
		Secrets:                    &policy.SecretRules{Allow: []string{"env/MARKETING_*", "k8s/marketing-*"}},
		SecretRuleKubeNamespace:    "caesium-marketing",
	}, got)

	// A mapping that names only a service account keeps the owner's defaults
	// as the effective namespaces.
	got, _, _ = ResolveIsolationContext("accounts")
	require.False(t, got.Default())
	require.Equal(t, &atom.KubeTarget{ServiceAccountName: "shared-jobs"}, got.Kubernetes)
	require.Equal(t, "owner-secrets", got.SecretReadNamespace)
	require.Equal(t, "owner-pods", got.SecretRuleKubeNamespace)
	require.Nil(t, got.Secrets)

	// Declared without settings (and the always-declared default): a default
	// context, still carried, still effective-target complete.
	for _, ns := range []string{"finance", "", "default"} {
		got, carry, _ = ResolveIsolationContext(ns)
		require.True(t, carry, ns)
		require.True(t, got.Default(), ns)
		require.Equal(t, models.NamespaceOrDefault(ns), got.Namespace)
		require.Equal(t, "owner-pods", got.DefaultKubernetesNamespace)
		require.Equal(t, "owner-secrets", got.SecretReadNamespace)
	}

	// An empty allow list denies everything and must survive the wire as a
	// present, empty rule set — never as "no rules" (allow all).
	got, _, _ = ResolveIsolationContext("locked")
	require.False(t, got.Default())
	wire, err := json.Marshal(got)
	require.NoError(t, err)
	var decoded IsolationContext
	require.NoError(t, json.Unmarshal(wire, &decoded))
	require.NotNil(t, decoded.Secrets)
	require.Empty(t, decoded.Secrets.Allow)
	require.False(t, decoded.Default())

	// Undeclared under a policy file: non-default, deny everything.
	got, carry, _ = ResolveIsolationContext("ghost")
	require.True(t, carry)
	require.True(t, got.Undeclared)
	require.False(t, got.Default())
	require.Equal(t, "ghost", got.Namespace)
}

func TestResolveIsolationContextWaitsForAConfiguredPolicy(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte(isolationPolicy), 0o600))
	setIsolationEnv(t, map[string]string{"CAESIUM_AUTH_POLICY_FILE": path})
	policy.SetProcessHolder(nil)
	_, carry, ready := ResolveIsolationContext("marketing")
	require.False(t, ready, "a configured file that is not installed yet holds dispatch")
	require.False(t, carry)
}

// TestIsolationKubernetesMappingMatchesInProcess is the parity oracle: a
// worker reading the mapping from a context the owner resolved gets exactly
// what the owner's own in-process resolution gives, refusals included.
func TestIsolationKubernetesMappingMatchesInProcess(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	setIsolationEnv(t, map[string]string{"CAESIUM_AUTH_POLICY_FILE": ""})
	policy.SetProcessHolder(loadIsolationPolicy(t, isolationPolicy))
	engines := []models.AtomEngine{models.AtomEngineKubernetes, models.AtomEngineDocker, models.AtomEnginePodman}
	for _, ns := range []string{"", "default", "marketing", "accounts", "locked", "finance", "ghost"} {
		ctx, carry, ready := ResolveIsolationContext(ns)
		require.True(t, carry && ready, ns)
		for _, engine := range engines {
			want, wantErr := KubernetesMapping(ns, engine, false)
			got, gotErr := ctx.KubernetesMapping(engine)
			require.Equal(t, want, got, "%s/%s", ns, engine)
			require.Equal(t, wantErr != nil, gotErr != nil, "%s/%s", ns, engine)
			if gotErr != nil {
				require.ErrorIs(t, gotErr, ErrKubernetesTargetDenied)
				require.Equal(t, incident.ClassConfig, incident.NewClassifier().Classify(incident.Signal{Error: gotErr.Error()}))
				var denied *KubernetesTargetError
				require.True(t, errors.As(gotErr, &denied))
			}
		}
	}
}

// TestIsolationPodTargetUsesTheOwnersDefaults: an unmapped Kubernetes pod runs
// in the namespace the owner resolved, whatever the worker's environment says.
func TestIsolationPodTargetUsesTheOwnersDefaults(t *testing.T) {
	setIsolationEnv(t, map[string]string{"CAESIUM_KUBERNETES_NAMESPACE": "worker-pods"})
	k8s, docker := models.AtomEngineKubernetes, models.AtomEngineDocker
	ctx := &IsolationContext{Namespace: "finance", DefaultKubernetesNamespace: "owner-pods"}
	require.Equal(t, atom.KubeTarget{Namespace: "owner-pods"}, ctx.PodTarget(k8s, atom.KubeTarget{}))
	require.Equal(t, atom.KubeTarget{Namespace: "owner-pods", ServiceAccountName: "sa"}, ctx.PodTarget(k8s, atom.KubeTarget{ServiceAccountName: "sa"}))
	require.Equal(t, atom.KubeTarget{Namespace: "mapped"}, ctx.PodTarget(k8s, atom.KubeTarget{Namespace: "mapped"}))
	require.True(t, ctx.PodTarget(docker, atom.KubeTarget{Namespace: "mapped"}).IsZero(), "other engines carry no pod target")
	rules := ctx.SecretRules()
	require.Equal(t, "finance", rules.Namespace)
	require.Nil(t, rules.Rules)
}

// TestIsolationSecretScopeMatchesInProcess is the secret parity oracle: the
// scope a worker builds from the owner's context allows exactly what the
// owner's in-process scope allows, for every reference form.
func TestIsolationSecretScopeMatchesInProcess(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	setIsolationEnv(t, map[string]string{
		"CAESIUM_AUTH_POLICY_FILE":              "",
		"CAESIUM_KUBERNETES_NAMESPACE":          "default",
		"CAESIUM_JOBDEF_SECRETS_KUBE_NAMESPACE": "default",
	})
	policy.SetProcessHolder(loadIsolationPolicy(t, isolationPolicy))
	inner := secret.NewMultiResolver(map[string]secret.Resolver{
		"env":        secret.NewEnvResolver(),
		"k8s":        secret.NewKubernetesResolverWithClient(fake.NewClientset(), "default"),
		"kubernetes": secret.NewKubernetesResolverWithClient(fake.NewClientset(), "default"),
	})
	refs := []string{
		"secret://env/MARKETING_TOKEN", "secret://env/FINANCE_TOKEN",
		"secret://k8s/marketing-db/password", "secret://k8s/caesium-marketing/marketing-db/password",
		"secret://k8s/default/marketing-db/password", "secret://kubernetes/marketing-db/password",
		"secret://k8s/other/x?namespace=caesium-marketing", "secret://env/X?name=MARKETING_TOKEN",
	}
	for _, ns := range []string{"default", "marketing", "accounts", "locked", "finance", "ghost"} {
		ctx, _, _ := ResolveIsolationContext(ns)
		mapping, _ := KubernetesMapping(ns, models.AtomEngineDocker, false)
		local := secret.ForNamespaceTarget(inner, ns, mapping.Namespace)
		dispatched := secret.ForIsolation(inner, ns, ctx.SecretReadNamespace, ctx.SecretRules(), nil)
		for _, ref := range refs {
			require.Equal(t, local.Allow(ref) == nil, dispatched.Allow(ref) == nil, "%s %s", ns, ref)
		}
	}
}
