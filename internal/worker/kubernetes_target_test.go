package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	jobdefruntime "github.com/caesium-cloud/caesium/internal/jobdef/runtime"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
)

func installDefaultMapping(t *testing.T, target atom.KubeTarget) {
	t.Helper()
	view := policy.EmptyPolicy()
	view.Namespaces[models.DefaultNamespace] = policy.Namespace{Kubernetes: &policy.KubernetesSettings{
		Namespace: target.Namespace, ServiceAccountName: target.ServiceAccountName,
	}}
	policy.SetProcessHolder(policy.NewEnvHolder(view, "", nil))
}

func setTaskEngine(t *testing.T, f fanOutTaskRunFixture, engine models.AtomEngine) {
	t.Helper()
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", f.taskRun.ID).Update("engine", engine).Error)
	f.taskRun.Engine = engine
}

// TestRuntimeExecutorCarriesTheKubernetesMapping: a Kubernetes task's create
// request carries the namespace mapping resolved for the execution, which is
// recorded on its descriptor and folded into its cache identity. Not
// parallel: it installs the process holder.
func TestRuntimeExecutorCarriesTheKubernetesMapping(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	target := atom.KubeTarget{Namespace: "caesium-marketing", ServiceAccountName: "marketing-jobs"}
	f := seedProducerTaskRun(t, "kube-mapped")
	setTaskEngine(t, f, models.AtomEngineKubernetes)
	installDefaultMapping(t, target)

	engine := &captureCreateEngine{}
	executor := &runtimeExecutor{store: f.store, localSink: &fakeSink{}, engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) {
		return engine, nil
	}}
	executor.Execute(context.Background(), f.taskRun)

	require.NotNil(t, engine.createReq)
	require.Equal(t, target, engine.createReq.Target)
	desc, err := f.store.TaskExecutionDescriptor(context.Background(), f.jobRun.ID, f.taskRun.ID)
	require.NoError(t, err)
	require.Equal(t, &target, desc.Runtime.KubernetesTarget)
	var row models.TaskRun
	require.NoError(t, f.db.First(&row, "id = ?", f.taskRun.ID).Error)
	require.Contains(t, string(row.HashInputBlob), `"kubernetesTarget":{"namespace":"caesium-marketing","serviceAccountName":"marketing-jobs"}`)
}

// TestRuntimeExecutorDockerTaskIgnoresThePodTarget: a Docker task in a mapped
// namespace gets no pod target and keeps its unmapped cache identity when it
// has no relative secret://k8s reference.
func TestRuntimeExecutorDockerTaskIgnoresThePodTarget(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	f := seedProducerTaskRun(t, "docker-mapped")
	installDefaultMapping(t, atom.KubeTarget{Namespace: "caesium-marketing"})

	engine := &captureCreateEngine{}
	executor := &runtimeExecutor{store: f.store, localSink: &fakeSink{}, engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) {
		return engine, nil
	}}
	executor.Execute(context.Background(), f.taskRun)
	require.NotNil(t, engine.createReq)
	require.True(t, engine.createReq.Target.IsZero())
	var row models.TaskRun
	require.NoError(t, f.db.First(&row, "id = ?", f.taskRun.ID).Error)
	require.NotContains(t, string(row.HashInputBlob), "kubernetes", "no pod target and no relative k8s read: identity unchanged")
}

// TestRuntimeExecutorRefusesAKubernetesTaskInAnUndeclaredNamespace: with a
// policy file served, a Kubernetes task whose namespace it does not declare
// fails as a config failure without creating a pod.
func TestRuntimeExecutorRefusesAKubernetesTaskInAnUndeclaredNamespace(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte("apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  finance: {}\n"), 0o600))
	holder, err := policy.LoadFile(context.Background(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	policy.SetProcessHolder(holder)

	f := seedProducerTaskRun(t, "kube-undeclared")
	require.NoError(t, f.db.Model(&models.JobRun{}).Where("id = ?", f.jobRun.ID).Update("namespace", "ghost").Error)
	setTaskEngine(t, f, models.AtomEngineKubernetes)

	engine := &captureCreateEngine{}
	sink := &fakeSink{}
	executor := &runtimeExecutor{store: f.store, localSink: sink, engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) {
		return engine, nil
	}}
	executor.Execute(context.Background(), f.taskRun)
	require.Nil(t, engine.createReq, "no pod is created")
	require.Equal(t, 1, sink.failed)
	require.ErrorIs(t, sink.lastErr, jobdefruntime.ErrKubernetesTargetDenied)
	require.True(t, strings.Contains(sink.lastErr.Error(), `"ghost"`))
}
