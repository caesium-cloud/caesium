package run

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestPostgresNamespaceQuotaAdmitsExactlyCap proves the namespace gate is
// race-free on PostgreSQL, where READ COMMITTED would let concurrent
// count-then-insert transactions overshoot: the transaction-scoped advisory
// lock serializes the gate per namespace. Opt-in via CAESIUM_TEST_POSTGRES_DSN.
func TestPostgresNamespaceQuotaAdmitsExactlyCap(t *testing.T) {
	db := openDeadlinePostgres(t)
	// openDeadlinePostgres migrates the run tables; add the rest of what
	// admission touches (models.All also holds SQLite-only blob columns).
	require.NoError(t, db.AutoMigrate(
		&models.RunQueue{},
		&models.RunStartIdempotency{},
		&models.DatasetDeclaration{},
		&models.DatasetHold{},
	))
	store := NewStore(db)
	useAccessPolicy(t, `apiVersion: v1
kind: AccessPolicy
namespaces:
  default: {}
  quota-a:
    quotas:
      maxConcurrentRuns: 2
`)
	const starts = 12
	now := time.Now().UTC()
	triggerID := uuid.New()
	require.NoError(t, db.Create(&models.Trigger{
		ID: triggerID, Alias: "pg-quota-trigger", Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now,
	}).Error)
	jobs := make([]uuid.UUID, starts)
	for i := range jobs {
		job := &models.Job{ID: uuid.New(), TriggerID: triggerID, Alias: fmt.Sprintf("pg-quota-%d", i), Namespace: "quota-a"}
		require.NoError(t, db.Create(job).Error)
		jobs[i] = job.ID
	}

	for round := range 3 {
		var wg sync.WaitGroup
		errs := make([]error, starts)
		start := make(chan struct{})
		for i := range jobs {
			wg.Go(func() {
				<-start
				_, errs[i] = store.Start(jobs[i], nil)
			})
		}
		close(start)
		wg.Wait()

		created := 0
		for _, err := range errs {
			switch {
			case err == nil:
				created++
			case errors.Is(err, ErrRunQueuedByQuota):
			default:
				t.Fatalf("round %d: unexpected start error: %v", round, err)
			}
		}
		require.Equal(t, 2, created, "round %d: exactly the cap is admitted", round)
		var running int64
		require.NoError(t, db.Model(&models.JobRun{}).Where("namespace = ? AND status = ?", "quota-a", string(StatusRunning)).Count(&running).Error)
		require.Equal(t, int64(2), running, "round %d", round)
		// Free the namespace for the next round.
		require.NoError(t, db.Model(&models.JobRun{}).Where("namespace = ? AND status = ?", "quota-a", string(StatusRunning)).
			Update("status", string(StatusSucceeded)).Error)
	}
}

// TestPostgresRetryLocksTheRunBeforeTheNamespaceQuota: a partition retry of a
// finished run holds the run row (FOR UPDATE) and then takes the namespace
// quota's advisory lock. A concurrent whole-run retry of the same run must
// take the locks in that same order — row first — so the two serialize. In
// the reverse order PostgreSQL aborts one of them with a deadlock (40P01).
func TestPostgresRetryLocksTheRunBeforeTheNamespaceQuota(t *testing.T) {
	db := openDeadlinePostgres(t)
	require.NoError(t, db.AutoMigrate(&models.RunQueue{}, &models.RunStartIdempotency{}, &models.DatasetDeclaration{}, &models.DatasetHold{}, &models.RunLease{}))
	// A retry clears the run's checkpoints. RunCheckpoint's SQLite blob tag
	// cannot be AutoMigrated on PostgreSQL (see sql_terminal_handoff_test.go).
	require.NoError(t, db.Exec(`CREATE TABLE run_checkpoints (
		run_id text NOT NULL, sequence_high bigint NOT NULL,
		owner_generation bigint NOT NULL, state_blob bytea NOT NULL,
		is_incremental boolean NOT NULL DEFAULT false, created_at timestamptz NOT NULL,
		PRIMARY KEY (run_id, sequence_high)
	)`).Error)
	store := NewStore(db)
	useAccessPolicy(t, `apiVersion: v1
kind: AccessPolicy
namespaces:
  default: {}
  quota-a:
    quotas:
      maxConcurrentRuns: 5
`)
	now := time.Now().UTC()
	triggerID := uuid.New()
	require.NoError(t, db.Create(&models.Trigger{
		ID: triggerID, Alias: "pg-retry-lock-trigger", Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now,
	}).Error)
	job := &models.Job{ID: uuid.New(), TriggerID: triggerID, Alias: "pg-retry-lock-order", Namespace: "quota-a"}
	require.NoError(t, db.Create(job).Error)
	runID := uuid.New()
	completed := now
	require.NoError(t, db.Create(&models.JobRun{
		ID: runID, JobID: job.ID, TriggerID: triggerID, Namespace: "quota-a", Status: string(StatusFailed),
		StartedAt: now.Add(-time.Minute), CompletedAt: &completed, CreatedAt: now.Add(-time.Minute), UpdatedAt: now,
	}).Error)

	// The partition retry's transaction, as RetryPartition runs it: the run
	// row first.
	partition := db.Begin()
	require.NoError(t, partition.Error)
	finished := false
	t.Cleanup(func() {
		if !finished {
			partition.Rollback()
		}
	})
	require.NoError(t, lockJobRunForPartitionRetryTx(partition, runID))

	done := make(chan error, 1)
	go func() {
		_, err := store.RetryFromFailure(runID)
		done <- err
	}()
	// Wait until the whole-run retry is blocked on a lock the partition
	// retry holds.
	require.Eventually(t, func() bool {
		var waiting int64
		return db.Raw("SELECT count(*) FROM pg_locks WHERE NOT granted").Scan(&waiting).Error == nil && waiting > 0
	}, 5*time.Second, 20*time.Millisecond, "the whole-run retry never waited on the partition retry")

	require.NoError(t, namespaceCapacityTx(partition, "quota-a"),
		"the partition retry reaches the namespace gate without a deadlock")
	require.NoError(t, partition.Rollback().Error)
	finished = true
	select {
	case err := <-done:
		require.NoError(t, err, "the whole-run retry proceeds once the partition retry ends")
	case <-time.After(10 * time.Second):
		t.Fatal("the whole-run retry never finished")
	}
	var stored models.JobRun
	require.NoError(t, db.First(&stored, "id = ?", runID).Error)
	require.Equal(t, string(StatusRunning), stored.Status)
}
