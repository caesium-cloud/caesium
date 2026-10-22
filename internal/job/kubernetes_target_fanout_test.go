package job

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestFannedKubernetesStepWithAnUnknownTargetFailsEachInstance: the
// namespace stops being declared by the served policy file while the producer
// runs, so the fanned Kubernetes step's target cannot be known. Each expanded
// instance fails on its own row as a config failure (never left pending, never
// a container), rather than the catalog task failing ambiguously. Not
// parallel: it installs the process holder.
func TestFannedKubernetesStepWithAnUnknownTargetFailsEachInstance(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	const namespace = "c1-fanned"
	f := newFanOutFixture(t, `["a","b"]`, &schema.FanOut{
		From: "list", MaxPartitions: 16, MaxParallel: 1, FailurePolicy: schema.FanOutFailureContinue,
	}, 2)
	require.NoError(t, f.db.Create(&models.Job{ID: f.jobID, Alias: namespace, Namespace: namespace}).Error)
	f.atomSvc.atoms[f.taskSvc.tasks[1].AtomID].Engine = models.AtomEngineKubernetes

	view := policy.EmptyPolicy()
	view.Namespaces[namespace] = policy.Namespace{}
	policy.SetProcessHolder(policy.NewEnvHolder(view, "", nil))
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte("apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  finance: {}\n"), 0o600))
	undeclared, err := policy.LoadFile(context.Background(), policy.HolderOptions{Path: path})
	require.NoError(t, err)

	engine := &hookEngine{fakeEngine: f.engine, onCreate: func(req *atom.EngineCreateRequest) {
		if req.Spec.Env[schema.DefaultFanOutEnv] == "" {
			policy.SetProcessHolder(undeclared) // while the producer runs
		}
	}}
	opts := withTestDeps(f.store, defaultFanOutVars(), f.taskSvc, f.atomSvc, f.edgeSvc, engine)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	require.Error(t, New(&models.Job{ID: f.jobID, Alias: namespace, Namespace: namespace}, opts...).Run(ctx))

	rows := f.instanceRows(t)
	require.Len(t, rows, 2)
	for _, row := range rows {
		require.Equal(t, string(run.TaskStatusFailed), row.Status, row.PartitionValue)
		require.Contains(t, row.Error, secret.DeniedMarker, row.PartitionValue)
		require.Contains(t, row.Error, "kubernetes target", row.PartitionValue)
		require.LessOrEqual(t, row.Attempt, 1, "%s: a refusal is never retried", row.PartitionValue)
		require.Zero(t, f.engine.createCallsByPartition[row.PartitionValue], row.PartitionValue)
	}
}

// TestLocalRetryClearsAMappingTheRowNoLongerRunsUnder: an instance that ran
// under a namespace mapping records it on its descriptor; once the policy no
// longer maps the namespace, its partition retry runs in the default target
// and clears the recorded mapping, so the descriptor (and a later replay's
// comparison) names what the latest execution used. Not parallel: it
// installs the process holder.
func TestLocalRetryClearsAMappingTheRowNoLongerRunsUnder(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	f := newFanOutFixture(t, `["ok","flaky"]`, &schema.FanOut{
		From: "list", MaxPartitions: 16, FailurePolicy: schema.FanOutFailureContinue,
	}, 0)
	f.atomSvc.atoms[f.taskSvc.tasks[1].AtomID].Engine = models.AtomEngineKubernetes
	view := policy.EmptyPolicy()
	view.Namespaces[models.DefaultNamespace] = policy.Namespace{Kubernetes: &policy.KubernetesSettings{Namespace: "caesium-a"}}
	policy.SetProcessHolder(policy.NewEnvHolder(view, "", nil))
	f.engine.createErrByPartition["flaky"] = fmt.Errorf("boom")

	require.Error(t, f.run(t, defaultFanOutVars()))
	jobRun := f.latestJobRun(t)
	var flakyID uuid.UUID
	for _, row := range f.instanceRows(t) {
		if row.PartitionValue == "flaky" {
			flakyID = row.ID
			require.Contains(t, string(row.ExecutionDescriptor), `"kubernetesTarget":{"namespace":"caesium-a"}`)
		}
	}
	require.NotEqual(t, uuid.Nil, flakyID)

	policy.SetProcessHolder(nil) // the mapping (and the policy) is gone
	f.engine.mu.Lock()
	delete(f.engine.createErrByPartition, "flaky")
	f.engine.mu.Unlock()
	_, reopened, err := f.store.RetryPartition(context.Background(), jobRun.ID, flakyID)
	require.NoError(t, err)
	require.True(t, reopened)
	require.NoError(t, f.resume(t, jobRun.ID))

	var retried models.TaskRun
	require.NoError(t, f.db.First(&retried, "id = ?", flakyID).Error)
	require.Equal(t, string(run.TaskStatusSucceeded), retried.Status)
	require.NotContains(t, string(retried.ExecutionDescriptor), "kubernetesTarget", "the retry ran unmapped and says so")
}
