package replay

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// mapDefaultNamespace installs a process policy that maps the default
// namespace (the replay fixture's) to target; a zero target installs none.
func mapDefaultNamespace(target atom.KubeTarget) {
	view := policy.EmptyPolicy()
	if !target.IsZero() {
		view.Namespaces[models.DefaultNamespace] = policy.Namespace{Kubernetes: &policy.KubernetesSettings{
			Namespace: target.Namespace, ServiceAccountName: target.ServiceAccountName,
		}}
	}
	policy.SetProcessHolder(policy.NewEnvHolder(view, "", nil))
}

// TestReplayReexecutesAStepWhoseKubernetesMappingMoved: a fully cached replay
// reuses a Kubernetes step only while the namespace still maps where the
// baseline ran; after a remap the step re-executes. A descriptor recorded
// before C1 (no mapping field) is the legacy default target. Not parallel: it
// installs the process holder.
func TestReplayReexecutesAStepWhoseKubernetesMappingMoved(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	mappedA := atom.KubeTarget{Namespace: "caesium-a", ServiceAccountName: "jobs"}

	for _, tc := range []struct {
		name      string
		recorded  *atom.KubeTarget
		current   atom.KubeTarget
		wantReuse bool
	}{
		{name: "same mapping is reused", recorded: &mappedA, current: mappedA, wantReuse: true},
		{name: "remapped namespace re-executes", recorded: &mappedA, current: atom.KubeTarget{Namespace: "caesium-b", ServiceAccountName: "jobs"}},
		{name: "changed default account re-executes", recorded: &mappedA, current: atom.KubeTarget{Namespace: "caesium-a", ServiceAccountName: "other"}},
		{name: "legacy descriptor, still unmapped, is reused", current: atom.KubeTarget{}, wantReuse: true},
		{name: "legacy descriptor, now mapped, re-executes", current: mappedA},
		{name: "mapping removed re-executes", recorded: &mappedA, current: atom.KubeTarget{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReplayFixture(t)
			f.seedTask(t, seedTaskConfig{name: "load", result: "success", replaySafe: true, engine: models.AtomEngineKubernetes, kubeTarget: tc.recorded})
			mapDefaultNamespace(tc.current)
			result, err := New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{BaselineRunID: f.runID})
			require.NoError(t, err)
			require.Len(t, result.Decisions, 1)
			require.Equal(t, tc.wantReuse, result.Decisions[0].CacheHit)
			require.Equal(t, !tc.wantReuse, result.Decisions[0].Reexecute)
		})
	}
}

// TestReplayDockerStepWithRelativeKubernetesSecretFollowsTheMapping: a
// Docker step reads relative secret://k8s references in its namespace's
// mapping, so a remap re-executes it; a Docker step without one is unaffected.
// Re-execution verifies the baseline secret identity by re-reading it where
// the re-execution will: in the current mapped namespace. Not parallel.
func TestReplayDockerStepWithRelativeKubernetesSecretFollowsTheMapping(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	const ref = "secret://k8s/token/value"
	client := fake.NewClientset(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "token", Namespace: "caesium-a", ResourceVersion: "11"}, Data: map[string][]byte{"value": []byte("a")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "token", Namespace: "default", ResourceVersion: "22"}, Data: map[string][]byte{"value": []byte("d")}},
	)
	kube := secret.NewKubernetesResolverWithClient(client, "default")
	resolver := WithSecretResolver(secret.NewMultiResolver(map[string]secret.Resolver{"k8s": kube, "kubernetes": kube}))
	baselineIdentity := secret.Identity{Provider: "k8s", Ref: ref, Namespace: "caesium-a", Name: "token", Key: "value", ResourceVersion: "11", Verifiable: true}
	mappedA := atom.KubeTarget{Namespace: "caesium-a"}

	// Unchanged mapping, params changed: re-executes, and verification reads
	// the baseline secret in caesium-a (the unmapped resolver would read the
	// "default" namespace's and refuse the replay as an identity change).
	f := newReplayFixture(t)
	f.seedTask(t, seedTaskConfig{
		name: "use", result: "success", replaySafe: true, kubeTarget: &mappedA,
		spec:       container.Spec{Env: map[string]string{"TOKEN": ref}},
		secretRefs: []models.TaskExecutionSecretRef{run.SecretIdentityDescriptorRef("TOKEN", ref, baselineIdentity)},
	})
	mapDefaultNamespace(mappedA)
	result, err := New(f.store, &recordingDispatcher{}, resolver).Replay(context.Background(), Request{BaselineRunID: f.runID, Set: map[string]string{"mode": "replayed"}})
	require.NoError(t, err)
	require.Len(t, result.Decisions, 1)
	require.True(t, result.Decisions[0].Reexecute)

	// Remapped: the step re-executes, and its baseline secret no longer is the
	// one the re-execution would read, so the replay is refused.
	mapDefaultNamespace(atom.KubeTarget{Namespace: "caesium-b"})
	_, err = New(f.store, &recordingDispatcher{}, resolver).Replay(context.Background(), Request{BaselineRunID: f.runID})
	require.ErrorIs(t, err, ErrSecretIdentity)

	// A Docker step without a relative reference is reused across the remap.
	g := newReplayFixture(t)
	g.seedTask(t, seedTaskConfig{name: "plain", result: "success", replaySafe: true, kubeTarget: &mappedA})
	result, err = New(g.store, &recordingDispatcher{}).Replay(context.Background(), Request{BaselineRunID: g.runID})
	require.NoError(t, err)
	require.True(t, result.Decisions[0].CacheHit)
}

func TestReplayKubernetesMappingChanged(t *testing.T) {
	t.Parallel()
	a := atom.KubeTarget{Namespace: "caesium-a", ServiceAccountName: "jobs"}
	desc := func(engine models.AtomEngine, recorded *atom.KubeTarget, env map[string]string) models.TaskExecutionDescriptor {
		return models.TaskExecutionDescriptor{Runtime: models.TaskExecutionRuntime{Engine: engine, KubernetesTarget: recorded}, ContainerSpec: container.Spec{Env: env}}
	}
	require.False(t, replayKubernetesMappingChanged(desc(models.AtomEngineKubernetes, &a, nil), a))
	require.True(t, replayKubernetesMappingChanged(desc(models.AtomEngineKubernetes, &a, nil), atom.KubeTarget{}))
	require.True(t, replayKubernetesMappingChanged(desc(models.AtomEngineKubernetes, nil, nil), a))
	require.False(t, replayKubernetesMappingChanged(desc(models.AtomEngineKubernetes, nil, nil), atom.KubeTarget{}))
	relative := map[string]string{"T": "secret://kubernetes/token/value"}
	require.True(t, replayKubernetesMappingChanged(desc(models.AtomEngineDocker, &a, relative), atom.KubeTarget{Namespace: "caesium-b"}))
	require.False(t, replayKubernetesMappingChanged(desc(models.AtomEngineDocker, &a, relative), atom.KubeTarget{Namespace: "caesium-a", ServiceAccountName: "x"}),
		"a Docker step depends only on the read namespace")
	require.False(t, replayKubernetesMappingChanged(desc(models.AtomEngineDocker, &a, map[string]string{"T": "plain"}), atom.KubeTarget{}))
}

// TestComputeDescriptorHashFoldsTheRecordedMapping: replay rebuilds the key
// the executor computed under the recorded mapping, and an unmapped
// descriptor's key is unchanged by the C1 fields.
func TestComputeDescriptorHashFoldsTheRecordedMapping(t *testing.T) {
	t.Parallel()
	base := models.TaskExecutionDescriptor{
		Baseline: models.TaskExecutionBaseline{JobAlias: "j", TaskName: "t"},
		Runtime:  models.TaskExecutionRuntime{Engine: models.AtomEngineKubernetes, Image: "busybox:1.36.1"},
	}
	unmapped, err := computeDescriptorHash(base, nil, nil, nil)
	require.NoError(t, err)
	zero := base
	zero.Runtime.KubernetesTarget = &atom.KubeTarget{}
	same, err := computeDescriptorHash(zero, nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, unmapped, same)
	mapped := base
	mapped.Runtime.KubernetesTarget = &atom.KubeTarget{Namespace: "caesium-a"}
	moved, err := computeDescriptorHash(mapped, nil, nil, nil)
	require.NoError(t, err)
	require.NotEqual(t, unmapped, moved)
}
