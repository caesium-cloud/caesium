package job

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// TestLocalKubernetesTaskCarriesTheNamespaceMapping: the local lane resolves
// the run namespace's mapping once per execution, passes it to Create, records
// it on the descriptor and folds it into the cache identity, so a remap
// re-keys the cache. Not parallel: it installs the process holder.
func TestLocalKubernetesTaskCarriesTheNamespaceMapping(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	const namespace = "c1-local"
	f := newSecretScopeFixture(t, namespace, models.Task{CacheConfig: datatypes.JSON("true")})
	for id, a := range f.atomSvc.atoms {
		a.Engine = models.AtomEngineKubernetes
		a.Spec = datatypes.JSON(`{"env":{"PLAIN":"x"}}`)
		require.NoError(t, f.db.Model(&models.Atom{}).Where("id = ?", id).Updates(map[string]any{"engine": models.AtomEngineKubernetes, "spec": a.Spec}).Error)
	}
	install := func(target atom.KubeTarget) {
		view := policy.EmptyPolicy()
		view.Namespaces[namespace] = policy.Namespace{Kubernetes: &policy.KubernetesSettings{Namespace: target.Namespace, ServiceAccountName: target.ServiceAccountName}}
		policy.SetProcessHolder(policy.NewEnvHolder(view, "", nil))
	}
	resolver := localStaticSecretResolver{}

	mappedA := atom.KubeTarget{Namespace: "caesium-a", ServiceAccountName: "jobs"}
	install(mappedA)
	require.NoError(t, f.run(t, resolver))
	creates := f.engine.createRequestsForTask(f.taskID)
	require.Len(t, creates, 1)
	require.Equal(t, mappedA, creates[0].Target)
	first := latestRunSnapshot(t, f.store, f.jobID)
	desc, err := f.store.TaskExecutionDescriptor(context.Background(), first.ID, f.taskID)
	require.NoError(t, err)
	require.Equal(t, &mappedA, desc.Runtime.KubernetesTarget)

	// Same mapping: the cache serves the second run.
	require.NoError(t, f.run(t, resolver))
	require.Equal(t, run.TaskStatusCached, taskRunByID(latestRunSnapshot(t, f.store, f.jobID), f.taskID).Status)

	// Remapped: a miss, and the new run's pod targets the new namespace.
	install(atom.KubeTarget{Namespace: "caesium-b"})
	require.NoError(t, f.run(t, resolver))
	require.NotEqual(t, run.TaskStatusCached, taskRunByID(latestRunSnapshot(t, f.store, f.jobID), f.taskID).Status, "a remap never reuses the old namespace's output")
	creates = f.engine.createRequestsForTask(f.taskID)
	require.Len(t, creates, 2)
	require.Equal(t, atom.KubeTarget{Namespace: "caesium-b"}, creates[1].Target)
}
