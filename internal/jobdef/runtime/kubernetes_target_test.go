package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/incident"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/stretchr/testify/require"
)

const kubernetesTargetPolicy = `apiVersion: v1
kind: AccessPolicy
namespaces:
  marketing:
    kubernetes: {namespace: caesium-marketing, serviceAccountName: marketing-jobs}
  accounts:
    kubernetes: {serviceAccountName: shared-jobs}
  finance: {}
`

func loadTargetPolicy(t *testing.T) *policy.Holder {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte(kubernetesTargetPolicy), 0o600))
	holder, err := policy.LoadFile(context.Background(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	return holder
}

// Not parallel: installs the process holder.
func TestKubernetesMapping(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	t.Setenv("CAESIUM_AUTH_POLICY_FILE", "")
	require.NoError(t, env.Process())
	t.Cleanup(func() { _ = env.Process() })
	k8s, docker := models.AtomEngineKubernetes, models.AtomEngineDocker

	// No policy: zero everywhere, so pods, ids, hashes and descriptors are unchanged.
	policy.SetProcessHolder(nil)
	for _, ns := range []string{"", "default", "marketing", "ghost"} {
		got, err := KubernetesMapping(ns, k8s, false)
		require.NoError(t, err)
		require.True(t, got.IsZero(), ns)
	}

	policy.SetProcessHolder(loadTargetPolicy(t))
	got, err := KubernetesMapping("marketing", k8s, false)
	require.NoError(t, err)
	require.Equal(t, atom.KubeTarget{Namespace: "caesium-marketing", ServiceAccountName: "marketing-jobs"}, got)
	got, err = KubernetesMapping("marketing", docker, false)
	require.NoError(t, err)
	require.Equal(t, "caesium-marketing", got.Namespace, "every engine learns the mapping (relative k8s secret reads)")
	require.True(t, PodTarget(docker, got).IsZero())
	require.Equal(t, got, PodTarget(k8s, got))
	got, err = KubernetesMapping("accounts", k8s, false)
	require.NoError(t, err)
	require.Equal(t, atom.KubeTarget{ServiceAccountName: "shared-jobs"}, got)
	for _, ns := range []string{"", "default", "finance"} {
		got, err = KubernetesMapping(ns, k8s, false)
		require.NoError(t, err)
		require.True(t, got.IsZero(), ns)
	}

	// Undeclared on a file-backed node: a Kubernetes task fails closed as a
	// config failure; other engines are left to the secret rules.
	_, err = KubernetesMapping("ghost", k8s, false)
	require.ErrorIs(t, err, ErrKubernetesTargetDenied)
	require.Equal(t, incident.ClassConfig, incident.NewClassifier().Classify(incident.Signal{Error: err.Error()}))
	var denied *KubernetesTargetError
	require.True(t, errors.As(err, &denied))
	require.Equal(t, "ghost", denied.Namespace)
	got, err = KubernetesMapping("ghost", docker, false)
	require.NoError(t, err)
	require.True(t, got.IsZero())

	// A client process never applies a server policy.
	got, err = KubernetesMapping("marketing", k8s, true)
	require.NoError(t, err)
	require.True(t, got.IsZero())
	_, err = KubernetesMapping("ghost", k8s, true)
	require.NoError(t, err)

	// An env-translated policy (no file) never refuses an undeclared namespace.
	policy.SetProcessHolder(policy.NewEnvHolder(nil, "", nil))
	_, err = KubernetesMapping("ghost", k8s, false)
	require.NoError(t, err)
}

func TestKubernetesIdentity(t *testing.T) {
	t.Parallel()
	mapped := atom.KubeTarget{Namespace: "caesium-marketing", ServiceAccountName: "marketing-jobs"}
	relative := map[string]string{"TOKEN": "secret://k8s/token/value"}
	absolute := map[string]string{"TOKEN": "secret://k8s/caesium-marketing/token/value"}

	target, ns := KubernetesIdentity(models.AtomEngineKubernetes, mapped, relative)
	require.Equal(t, &mapped, target)
	require.Empty(t, ns, "the pod target already names the read namespace")
	target, ns = KubernetesIdentity(models.AtomEngineKubernetes, atom.KubeTarget{}, relative)
	require.Nil(t, target)
	require.Empty(t, ns)

	target, ns = KubernetesIdentity(models.AtomEngineDocker, mapped, relative)
	require.Nil(t, target)
	require.Equal(t, "caesium-marketing", ns)
	_, ns = KubernetesIdentity(models.AtomEngineDocker, mapped, absolute)
	require.Empty(t, ns, "an absolute reference does not depend on the mapping")
	_, ns = KubernetesIdentity(models.AtomEnginePodman, atom.KubeTarget{ServiceAccountName: "x"}, relative)
	require.Empty(t, ns)

	require.True(t, RecordedKubernetesMapping(nil).IsZero())
	require.Equal(t, mapped, RecordedKubernetesMapping(&mapped))
}

func TestKubernetesNamespaces(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	t.Setenv("CAESIUM_KUBERNETES_NAMESPACE", "caesium")
	require.NoError(t, env.Process())
	t.Cleanup(func() { _ = env.Process() })
	policy.SetProcessHolder(nil)
	require.Equal(t, []string{"caesium"}, KubernetesNamespaces())
	policy.SetProcessHolder(loadTargetPolicy(t))
	require.Equal(t, []string{"caesium", "caesium-marketing"}, KubernetesNamespaces())
}
