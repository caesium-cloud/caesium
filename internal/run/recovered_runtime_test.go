package run

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/atom/kubernetes"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func recoveryPod(namespace, name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
}

// useFakeRecoveryCluster points recovery stops at a fake cluster holding pods
// and makes them synchronous.
func useFakeRecoveryCluster(t *testing.T, pods ...*corev1.Pod) *fake.Clientset {
	t.Helper()
	objects := make([]k8sruntime.Object, 0, len(pods))
	for _, pod := range pods {
		objects = append(objects, pod)
	}
	client := fake.NewClientset(objects...)
	t.Cleanup(SetRecoveryEngineForTesting(func(ctx context.Context) (atom.Engine, error) {
		return kubernetes.NewEngine(ctx, client.CoreV1())
	}))
	return client
}

func podExists(t *testing.T, client *fake.Clientset, namespace, name string) bool {
	t.Helper()
	_, err := client.CoreV1().Pods(namespace).Get(context.Background(), name, metav1.GetOptions{})
	return err == nil
}

// markKubernetesRow turns a seeded row into an in-flight Kubernetes attempt
// with a persisted runtime id.
func markKubernetesRow(t *testing.T, db *gorm.DB, runID, taskID uuid.UUID, runtimeID string, claimExpiresAt time.Time) {
	t.Helper()
	require.NoError(t, db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		Updates(map[string]any{
			"engine":           string(models.AtomEngineKubernetes),
			"status":           string(TaskStatusRunning),
			"claimed_by":       "node-dead",
			"claim_expires_at": claimExpiresAt,
			"runtime_id":       runtimeID,
		}).Error)
}

func TestStopRecoveredRuntimesUsesTheRuntimeIDsNamespace(t *testing.T) {
	client := useFakeRecoveryCluster(t,
		recoveryPod("caesium-mapped", "step-a"),
		recoveryPod("default", "step-legacy"),
		recoveryPod("caesium-other", "step-a"),
	)

	StopRecoveredRuntimes("test", []RecoveredRuntime{
		// A missing pod is logged and never stops the rest.
		{TaskRunID: uuid.New(), Engine: models.AtomEngineKubernetes, RuntimeID: "caesium-mapped/gone"},
		{TaskRunID: uuid.New(), Engine: models.AtomEngineKubernetes, RuntimeID: "caesium-mapped/step-a"},
		// A legacy unqualified id resolves in CAESIUM_KUBERNETES_NAMESPACE.
		{TaskRunID: uuid.New(), Engine: models.AtomEngineKubernetes, RuntimeID: "step-legacy"},
	})

	require.False(t, podExists(t, client, "caesium-mapped", "step-a"), "the qualified id's pod is stopped in its own namespace")
	require.False(t, podExists(t, client, "default", "step-legacy"), "a legacy id's pod is stopped in the default namespace")
	require.True(t, podExists(t, client, "caesium-other", "step-a"), "a same-named pod in another namespace is untouched")
}

func TestStopRecoveredRuntimesToleratesAnUnavailableEngine(t *testing.T) {
	calls := 0
	t.Cleanup(SetRecoveryEngineForTesting(func(context.Context) (atom.Engine, error) {
		calls++
		return nil, errors.New("no cluster")
	}))
	StopRecoveredRuntimes("test", nil)
	require.Zero(t, calls, "nothing to stop never builds an engine")
	StopRecoveredRuntimes("test", []RecoveredRuntime{{TaskRunID: uuid.New(), Engine: models.AtomEngineKubernetes, RuntimeID: "ns/pod"}})
	require.Equal(t, 1, calls)
}

func TestRecoveredRuntimesFromKeepsOnlyResetKubernetesRows(t *testing.T) {
	kept, other, docker, blank := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	rows := []models.TaskRun{
		{ID: kept, Engine: models.AtomEngineKubernetes, RuntimeID: "ns/a"},
		{ID: other, Engine: models.AtomEngineKubernetes, RuntimeID: "ns/b"},
		{ID: docker, Engine: models.AtomEngineDocker, RuntimeID: "0123abcd"},
		{ID: blank, Engine: models.AtomEngineKubernetes, RuntimeID: " "},
	}
	got := recoveredRuntimesFrom(rows, uuidSet([]uuid.UUID{kept, docker, blank}))
	require.Equal(t, []RecoveredRuntime{{TaskRunID: kept, Engine: models.AtomEngineKubernetes, RuntimeID: "ns/a"}}, got)
	require.Empty(t, recoveredRuntimesFrom(rows, nil), "a stop list never outruns the reset")
}

// A reset with nothing to stop adds no statement to the reclaim tick.
func TestRecoveredRuntimesAfterResetSkipsTheReadWithNothingToStop(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	closed, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, closed.Close())

	rows := []models.TaskRun{
		{ID: uuid.New(), Engine: models.AtomEngineDocker, RuntimeID: "0123abcd"},
		{ID: uuid.New(), Engine: models.AtomEngineKubernetes},
	}
	got, err := RecoveredRuntimesAfterReset(db, rows)
	require.NoError(t, err, "a closed database proves no statement ran")
	require.Empty(t, got)
	_, err = RecoveredRuntimesAfterReset(db, []models.TaskRun{{ID: uuid.New(), Engine: models.AtomEngineKubernetes, RuntimeID: "ns/a"}})
	require.Error(t, err, "a real stop list reads, and its failure is the transaction's")
}

// Owner takeover re-pends the dead owner's in-flight rows; the previous
// attempt's pod must go with them, or the re-dispatch runs beside an orphan.
func TestOwnerTakeoverStopsThePreviousAttemptsPod(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, taskA, _ := seedTwoTaskRun(t, db, store, "node-dead")
	client := useFakeRecoveryCluster(t, recoveryPod("caesium-mapped", "step-a-1"))
	// The claim is still unexpired: takeover resets it regardless, which
	// fences the attempt (its worker's renewal loses the claim).
	markKubernetesRow(t, db, runID, taskA, "caesium-mapped/step-a-1", time.Now().UTC().Add(time.Hour))

	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	_, err := mgr.Recover(runID, 1)
	require.NoError(t, err)

	require.False(t, podExists(t, client, "caesium-mapped", "step-a-1"))
	row := taskRunFor(t, db, runID, taskA)
	require.Equal(t, string(TaskStatusPending), row.Status)
	require.Empty(t, row.RuntimeID)
}

func TestResetInFlightTasksReportsOnlyTheRowsItReset(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, taskA, taskB := seedTwoTaskRun(t, db, store, "")
	markKubernetesRow(t, db, runID, taskA, "caesium-mapped/step-a-1", time.Now().UTC().Add(time.Hour))
	// A Docker attempt is re-pended but its container id is host-local.
	require.NoError(t, db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", runID, taskB).
		Updates(map[string]any{"status": string(TaskStatusRunning), "runtime_id": "0123abcd"}).Error)

	recovered, err := store.ResetOwnerInFlightTasks(runID, 1)
	require.NoError(t, err)
	require.Len(t, recovered, 1)
	require.Equal(t, "caesium-mapped/step-a-1", recovered[0].RuntimeID)
	require.Equal(t, string(TaskStatusPending), taskRunFor(t, db, runID, taskB).Status)

	again, err := store.ResetOwnerInFlightTasks(runID, 1)
	require.NoError(t, err)
	require.Empty(t, again, "nothing in flight, nothing to stop")
}

// The local resume stops nothing: it gets no stop list to act on, whatever the
// row's generation.
func TestResetInFlightTasksReturnsNoStopList(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, taskA, _ := seedTwoTaskRun(t, db, store, "")
	markKubernetesRow(t, db, runID, taskA, "caesium-mapped/step-a-1", time.Now().UTC().Add(time.Hour))

	recovered, err := store.resetInFlightTasks(runID, noRecoveryOwner)
	require.NoError(t, err)
	require.Empty(t, recovered)
	require.Equal(t, string(TaskStatusPending), taskRunFor(t, db, runID, taskA).Status)
}

// A completion landing between ResetOwnerInFlightTasks' read and its UPDATE
// leaves the row terminal, and it stays out of the stop list. The seam commits
// that write through the reset's own transaction: SQLite cannot interleave a
// second writer, and on Postgres READ COMMITTED shows the UPDATE the same state.
func TestOwnerTakeoverSparesARowThatCompletedAfterTheRead(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, taskA, taskB := seedTwoTaskRun(t, db, store, "")
	client := useFakeRecoveryCluster(t,
		recoveryPod("caesium-mapped", "step-a-1"),
		recoveryPod("caesium-mapped", "step-b-1"),
	)
	markKubernetesRow(t, db, runID, taskA, "caesium-mapped/step-a-1", time.Now().UTC().Add(time.Hour))
	markKubernetesRow(t, db, runID, taskB, "caesium-mapped/step-b-1", time.Now().UTC().Add(time.Hour))
	store.afterRecoveryRowsRead = func(tx *gorm.DB) {
		require.NoError(t, tx.Model(&models.TaskRun{}).
			Where("job_run_id = ? AND task_id = ?", runID, taskA).
			Update("status", string(TaskStatusSucceeded)).Error)
	}

	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	_, err := mgr.Recover(runID, 1)
	require.NoError(t, err)

	require.True(t, podExists(t, client, "caesium-mapped", "step-a-1"), "the completed row was not reset, so its pod is not recovery's to stop")
	require.False(t, podExists(t, client, "caesium-mapped", "step-b-1"), "the row the reset landed on is stopped")
	require.Equal(t, string(TaskStatusSucceeded), taskRunFor(t, db, runID, taskA).Status)
}

// The reset has no claim or generation guard of its own, so the stop carries
// the generation fence: an owner that lost its lease must not force-stop the
// attempt of the newer owner holding it, while its own generation's and older
// rows are stopped.
func TestOwnerTakeoverNeverStopsANewerGenerationsAttempt(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, taskA, taskB := seedTwoTaskRun(t, db, store, "")
	client := useFakeRecoveryCluster(t,
		recoveryPod("caesium-mapped", "step-a-1"),
		recoveryPod("caesium-mapped", "step-b-1"),
	)
	markKubernetesRow(t, db, runID, taskA, "caesium-mapped/step-a-1", time.Now().UTC().Add(time.Hour))
	markKubernetesRow(t, db, runID, taskB, "caesium-mapped/step-b-1", time.Now().UTC().Add(time.Hour))
	for taskID, generation := range map[uuid.UUID]int64{taskA: 2, taskB: 1} {
		require.NoError(t, db.Model(&models.TaskRun{}).
			Where("job_run_id = ? AND task_id = ?", runID, taskID).
			Update("owner_generation", generation).Error)
	}

	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	_, err := mgr.Recover(runID, 1)
	require.NoError(t, err)

	require.True(t, podExists(t, client, "caesium-mapped", "step-a-1"), "a newer generation's attempt is never force-stopped")
	require.False(t, podExists(t, client, "caesium-mapped", "step-b-1"), "this generation's attempt is stopped")
}

// The owner's reap stops the lapsed attempt's pod and never a live claim's.
func TestReclaimOwnerExpiredClaimsStopsOnlyTheLapsedAttemptsPod(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, taskA, taskB := seedTwoTaskRun(t, db, store, "")
	client := useFakeRecoveryCluster(t,
		recoveryPod("caesium-mapped", "step-a-1"),
		recoveryPod("caesium-mapped", "step-b-1"),
	)
	markKubernetesRow(t, db, runID, taskA, "caesium-mapped/step-a-1", time.Now().UTC().Add(-time.Minute))
	markKubernetesRow(t, db, runID, taskB, "caesium-mapped/step-b-1", time.Now().UTC().Add(time.Hour))

	rows, err := store.ReclaimOwnerExpiredClaims(runID, 1)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	require.False(t, podExists(t, client, "caesium-mapped", "step-a-1"), "the lapsed attempt's pod is stopped")
	require.True(t, podExists(t, client, "caesium-mapped", "step-b-1"), "a live claim's pod is never stopped")
	require.Equal(t, string(TaskStatusRunning), taskRunFor(t, db, runID, taskB).Status)
}

// A renewal landing between the owner reap's read and its guarded UPDATE keeps
// the claim, so the row stays out of the stop list. See
// TestOwnerTakeoverSparesARowThatCompletedAfterTheRead for how the seam stands
// in for the concurrent commit.
func TestReclaimOwnerExpiredClaimsSparesAClaimRenewedAfterTheRead(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, taskA, taskB := seedTwoTaskRun(t, db, store, "")
	client := useFakeRecoveryCluster(t,
		recoveryPod("caesium-mapped", "step-a-1"),
		recoveryPod("caesium-mapped", "step-b-1"),
	)
	lapsed := time.Now().UTC().Add(-time.Minute)
	markKubernetesRow(t, db, runID, taskA, "caesium-mapped/step-a-1", lapsed)
	markKubernetesRow(t, db, runID, taskB, "caesium-mapped/step-b-1", lapsed)
	store.afterRecoveryRowsRead = func(tx *gorm.DB) {
		require.NoError(t, tx.Model(&models.TaskRun{}).
			Where("job_run_id = ? AND task_id = ?", runID, taskA).
			Update("claim_expires_at", time.Now().UTC().Add(time.Hour)).Error)
	}

	_, err := store.ReclaimOwnerExpiredClaims(runID, 1)
	require.NoError(t, err)

	require.True(t, podExists(t, client, "caesium-mapped", "step-a-1"), "a renewed claim's pod is never stopped")
	renewed := taskRunFor(t, db, runID, taskA)
	require.Equal(t, string(TaskStatusRunning), renewed.Status)
	require.Equal(t, "caesium-mapped/step-a-1", renewed.RuntimeID)
	require.False(t, podExists(t, client, "caesium-mapped", "step-b-1"), "the row the reset landed on is stopped")
}
