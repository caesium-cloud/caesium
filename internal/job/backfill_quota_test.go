package job

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/metrics"
	metricstest "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	runstore "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/internal/runlife"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// The backfill fixture's job lives in "default"; cap it at one running run.
const backfillQuotaPolicy = `apiVersion: v1
kind: AccessPolicy
namespaces:
  default:
    quotas:
      maxConcurrentRuns: 1
`

func useBackfillQuotaPolicy(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte(backfillQuotaPolicy), 0o600))
	holder, err := policy.LoadFile(t.Context(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	previous := policy.ProcessHolder()
	t.Cleanup(func() { policy.SetProcessHolder(previous) })
	policy.SetProcessHolder(holder)

	base, maxBackoff := backfillQuotaBackoffBase, backfillQuotaBackoffMax
	backfillQuotaBackoffBase, backfillQuotaBackoffMax = 20*time.Millisecond, 80*time.Millisecond
	t.Cleanup(func() { backfillQuotaBackoffBase, backfillQuotaBackoffMax = base, maxBackoff })
}

// occupyDefaultNamespace seeds a running run of another job, filling the
// default namespace's single slot, and returns a func that frees it.
func occupyDefaultNamespace(t *testing.T, db *gorm.DB) func() {
	t.Helper()
	other := &models.Job{ID: uuid.New(), Alias: "backfill-quota-occupant-" + uuid.NewString()}
	require.NoError(t, db.Create(other).Error)
	now := time.Now().UTC()
	runID := uuid.New()
	require.NoError(t, db.Create(&models.JobRun{
		ID: runID, JobID: other.ID, Namespace: models.DefaultNamespace, Status: string(runstore.StatusRunning),
		StartedAt: now, CreatedAt: now, UpdatedAt: now,
	}).Error)
	return func() {
		require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", runID).Update("status", string(runstore.StatusSucceeded)).Error)
	}
}

// finishingExecute completes each child run it is handed, tracking how many
// were in flight at once.
func finishingExecute(t *testing.T, db *gorm.DB, executed, inFlight, maxInFlight *atomic.Int32) func(context.Context, *models.Job, map[string]string) error {
	return func(ctx context.Context, _ *models.Job, _ map[string]string) error {
		executed.Add(1)
		now := inFlight.Add(1)
		for {
			prev := maxInFlight.Load()
			if now <= prev || maxInFlight.CompareAndSwap(prev, now) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		inFlight.Add(-1)
		id, ok := runstore.FromContext(ctx)
		require.True(t, ok)
		return db.Model(&models.JobRun{}).Where("id = ?", id).Update("status", string(runstore.StatusSucceeded)).Error
	}
}

func awaitQuotaDeferral(t *testing.T, before float64) {
	t.Helper()
	require.Eventually(t, func() bool {
		return metricstest.CounterValue(t, metrics.NamespaceQuotaDeferralsTotal, models.DefaultNamespace) > before
	}, 5*time.Second, 10*time.Millisecond, "the backfill never waited on its namespace quota")
}

// TestBackfillWaitsOnNamespaceQuotaThenCompletes: a date the quota refuses is
// retried, never queued and never counted failed; once the slot frees every
// date runs, never more than the quota at once, and the accounting is exact.
func TestBackfillWaitsOnNamespaceQuotaThenCompletes(t *testing.T) {
	b, j, bStore, rStore := backfillLifetimeFixture(t)
	db := rStore.DB()
	require.NoError(t, db.Model(&models.Backfill{}).Where("id = ?", b.ID).Update("max_concurrent", 2).Error)
	b.MaxConcurrent = 2
	useBackfillQuotaPolicy(t)
	free := occupyDefaultNamespace(t, db)
	before := metricstest.CounterValue(t, metrics.NamespaceQuotaDeferralsTotal, models.DefaultNamespace)

	var executed, inFlight, maxInFlight atomic.Int32
	owner := runlife.New(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runBackfill(runlife.WithSupervisor(t.Context(), owner), b, j, mustParseCron(t, "0 * * * *"), time.UTC, bStore, rStore,
			finishingExecute(t, db, &executed, &inFlight, &maxInFlight))
	}()
	awaitQuotaDeferral(t, before)
	require.Zero(t, executed.Load(), "nothing runs while the namespace is full")
	free()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the backfill did not finish after its namespace freed")
	}
	owner.CloseAndCancel()

	require.EqualValues(t, 2, executed.Load())
	require.EqualValues(t, 1, maxInFlight.Load(), "the quota of one holds across the backfill's own maxConcurrent of two")
	stored, err := bStore.Get(b.ID)
	require.NoError(t, err)
	require.Equal(t, models.BackfillStatusSucceeded, stored.Status)
	require.Equal(t, 2, stored.TotalRuns)
	require.Equal(t, 2, stored.CompletedRuns)
	require.Zero(t, stored.FailedRuns)
	var queued int64
	require.NoError(t, db.Model(&models.RunQueue{}).Count(&queued).Error)
	require.Zero(t, queued, "a backfill child is never queued")
}

// TestBackfillCancelledWhileHeldEndsCancelled: a cancellation while a date is
// quota-held stops the driver; the backfill ends cancelled with nothing run
// and nothing counted.
func TestBackfillCancelledWhileHeldEndsCancelled(t *testing.T) {
	b, j, bStore, rStore := backfillLifetimeFixture(t)
	db := rStore.DB()
	useBackfillQuotaPolicy(t)
	occupyDefaultNamespace(t, db)
	before := metricstest.CounterValue(t, metrics.NamespaceQuotaDeferralsTotal, models.DefaultNamespace)

	var executed, inFlight, maxInFlight atomic.Int32
	owner := runlife.New(context.Background())
	t.Cleanup(owner.CloseAndCancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runBackfill(runlife.WithSupervisor(t.Context(), owner), b, j, mustParseCron(t, "0 * * * *"), time.UTC, bStore, rStore,
			finishingExecute(t, db, &executed, &inFlight, &maxInFlight))
	}()
	awaitQuotaDeferral(t, before)
	require.NoError(t, bStore.RequestCancel(b.ID))
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a held backfill did not stop on cancellation")
	}

	require.Zero(t, executed.Load())
	stored, err := bStore.Get(b.ID)
	require.NoError(t, err)
	require.Equal(t, models.BackfillStatusCancelled, stored.Status)
	require.Zero(t, stored.CompletedRuns)
	require.Zero(t, stored.FailedRuns)
	var children int64
	require.NoError(t, db.Model(&models.JobRun{}).Where("backfill_id = ?", b.ID).Count(&children).Error)
	require.Zero(t, children)
}

// TestBackfillShutdownWhileHeldKeepsDriverState: a server shutdown while a
// date is quota-held releases the driver's transient ownership but leaves the
// backfill running — not cancelled, nothing counted — so recovery resumes
// it; a later driver completes every date.
func TestBackfillShutdownWhileHeldKeepsDriverState(t *testing.T) {
	b, j, bStore, rStore := backfillLifetimeFixture(t)
	db := rStore.DB()
	useBackfillQuotaPolicy(t)
	free := occupyDefaultNamespace(t, db)
	before := metricstest.CounterValue(t, metrics.NamespaceQuotaDeferralsTotal, models.DefaultNamespace)

	var executed, inFlight, maxInFlight atomic.Int32
	execute := finishingExecute(t, db, &executed, &inFlight, &maxInFlight)
	owner := runlife.New(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runBackfill(runlife.WithSupervisor(t.Context(), owner), b, j, mustParseCron(t, "0 * * * *"), time.UTC, bStore, rStore, execute)
	}()
	awaitQuotaDeferral(t, before)
	// What cmd/start's shutdown does: close admission with the shutdown cause
	// (a bare CloseAndCancel is an ordinary cancellation, not a shutdown).
	owner.CloseAndCancelCause(runlife.ErrServerShutdown)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a held backfill did not release on server shutdown")
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, owner.Wait(waitCtx), "the held driver must not keep a reservation past shutdown")

	stored, err := bStore.Get(b.ID)
	require.NoError(t, err)
	require.Equal(t, models.BackfillStatusRunning, stored.Status, "shutdown is not a cancellation: recovery resumes the driver")
	require.Nil(t, stored.CancelRequestedAt)
	require.Zero(t, stored.CompletedRuns)
	require.Zero(t, stored.FailedRuns)
	require.Zero(t, executed.Load())

	// Recovery: a fresh driver after the restart completes the backfill.
	free()
	next := runlife.New(context.Background())
	t.Cleanup(next.CloseAndCancel)
	runBackfill(runlife.WithSupervisor(t.Context(), next), b, j, mustParseCron(t, "0 * * * *"), time.UTC, bStore, rStore, execute)
	stored, err = bStore.Get(b.ID)
	require.NoError(t, err)
	require.Equal(t, models.BackfillStatusSucceeded, stored.Status)
	require.Equal(t, 2, stored.CompletedRuns)
	require.EqualValues(t, 2, executed.Load())
}
