package worker

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/atom/kubernetes"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// A worker that died mid-task leaves its Kubernetes pod running. The reclaim
// that re-pends the row stops that pod — through the persisted, namespace-
// qualified runtime id — and never the pod of a claim that is still live.
func TestClaimerReclaimExpiredStopsTheExpiredAttemptsPod(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	client := fake.NewClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "caesium-mapped", Name: "step-expired"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "caesium-mapped", Name: "step-live"}},
	)
	t.Cleanup(run.SetRecoveryEngineForTesting(func(ctx context.Context) (atom.Engine, error) {
		return kubernetes.NewEngine(ctx, client.CoreV1())
	}))

	now := time.Now().UTC()
	expired := seedTaskRun(t, db, seedTaskRunInput{
		status:         string(run.TaskStatusRunning),
		claimedBy:      "node-old",
		claimExpiresAt: new(now.Add(-time.Minute)),
		claimAttempt:   1,
		createdAt:      now.Add(-2 * time.Minute),
	})
	live := seedTaskRun(t, db, seedTaskRunInput{
		status:         string(run.TaskStatusRunning),
		claimedBy:      "node-live",
		claimExpiresAt: new(now.Add(2 * time.Minute)),
		claimAttempt:   1,
		createdAt:      now.Add(-2 * time.Minute),
	})
	for id, runtimeID := range map[any]string{expired.ID: "caesium-mapped/step-expired", live.ID: "caesium-mapped/step-live"} {
		require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", id).
			Updates(map[string]any{"engine": string(models.AtomEngineKubernetes), "runtime_id": runtimeID}).Error)
	}

	claimer := NewClaimer("node-a", run.NewStore(db), time.Minute)
	require.NoError(t, claimer.ReclaimExpired(context.Background()))

	_, err := client.CoreV1().Pods("caesium-mapped").Get(context.Background(), "step-expired", metav1.GetOptions{})
	require.Error(t, err, "the expired attempt's pod is stopped")
	_, err = client.CoreV1().Pods("caesium-mapped").Get(context.Background(), "step-live", metav1.GetOptions{})
	require.NoError(t, err, "a live claim's pod is never stopped")

	var row models.TaskRun
	require.NoError(t, db.First(&row, "id = ?", expired.ID).Error)
	require.Equal(t, string(run.TaskStatusPending), row.Status)
	require.Empty(t, row.RuntimeID)
}

// On Postgres at READ COMMITTED a worker can renew its claim after the reclaim's
// read and before its guarded UPDATE: the UPDATE skips the row, which is still
// in the read's result, and recovery must not force-stop that live attempt. The
// seam commits the renewal through the reclaim's own transaction: SQLite cannot
// interleave a second writer, and the UPDATE sees the same state either way.
func TestClaimerReclaimExpiredSparesAClaimRenewedAfterTheRead(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	client := fake.NewClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "caesium-mapped", Name: "step-renewed"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "caesium-mapped", Name: "step-lapsed"}},
	)
	t.Cleanup(run.SetRecoveryEngineForTesting(func(ctx context.Context) (atom.Engine, error) {
		return kubernetes.NewEngine(ctx, client.CoreV1())
	}))

	now := time.Now().UTC()
	seed := func() *models.TaskRun {
		return seedTaskRun(t, db, seedTaskRunInput{
			status:         string(run.TaskStatusRunning),
			claimedBy:      "node-old",
			claimExpiresAt: new(now.Add(-time.Minute)),
			claimAttempt:   1,
			createdAt:      now.Add(-2 * time.Minute),
		})
	}
	renewed, lapsed := seed(), seed()
	for id, runtimeID := range map[any]string{renewed.ID: "caesium-mapped/step-renewed", lapsed.ID: "caesium-mapped/step-lapsed"} {
		require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", id).
			Updates(map[string]any{"engine": string(models.AtomEngineKubernetes), "runtime_id": runtimeID}).Error)
	}

	claimer := NewClaimer("node-a", run.NewStore(db), time.Minute)
	claimer.afterExpiredRowsRead = func(tx *gorm.DB) {
		require.NoError(t, tx.Model(&models.TaskRun{}).Where("id = ?", renewed.ID).
			Update("claim_expires_at", now.Add(time.Hour)).Error)
	}
	require.NoError(t, claimer.ReclaimExpired(context.Background()))

	_, err := client.CoreV1().Pods("caesium-mapped").Get(context.Background(), "step-renewed", metav1.GetOptions{})
	require.NoError(t, err, "a renewed claim's pod is never stopped")
	var row models.TaskRun
	require.NoError(t, db.First(&row, "id = ?", renewed.ID).Error)
	require.Equal(t, string(run.TaskStatusRunning), row.Status)
	require.Equal(t, "caesium-mapped/step-renewed", row.RuntimeID)

	_, err = client.CoreV1().Pods("caesium-mapped").Get(context.Background(), "step-lapsed", metav1.GetOptions{})
	require.Error(t, err, "the row the reset landed on is stopped")
}
