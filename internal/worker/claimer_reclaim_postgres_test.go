package worker

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// openReclaimPostgres opens a schema-isolated PostgreSQL catalog for the
// reclaim race tests. It is opt-in: the unit suite has no PostgreSQL server.
func openReclaimPostgres(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("CAESIUM_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("CAESIUM_TEST_POSTGRES_DSN is not set; PostgreSQL reclaim test is opt-in")
	}
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	adminSQL, err := admin.DB()
	require.NoError(t, err)
	schema := "caesium_reclaim_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	require.NoError(t, admin.Exec("CREATE SCHEMA "+schema).Error)

	scopedDSN := strings.TrimSpace(dsn) + " search_path=" + schema
	if strings.Contains(dsn, "://") {
		parsed, err := url.Parse(dsn)
		require.NoError(t, err)
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		scopedDSN = parsed.String()
	}
	db, err := gorm.Open(postgres.Open(scopedDSN), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(8)
	t.Cleanup(func() {
		require.NoError(t, sqlDB.Close())
		require.NoError(t, admin.Exec("DROP SCHEMA "+schema+" CASCADE").Error)
		require.NoError(t, adminSQL.Close())
	})
	require.NoError(t, db.AutoMigrate(
		&models.Atom{}, &models.Trigger{}, &models.Job{}, &models.Backfill{},
		&models.Task{}, &models.JobRun{}, &models.TaskRun{}, &models.RunLease{},
		&models.ExecutionEvent{},
	))
	return db
}

// seedPostgresLapsedRun seeds a running run with n in-flight rows whose worker
// claims lapsed a minute ago.
func seedPostgresLapsedRun(t *testing.T, db *gorm.DB, claimedBy string, n int) (uuid.UUID, []uuid.UUID) {
	t.Helper()
	now := time.Now().UTC()
	trigger := &models.Trigger{ID: uuid.New(), Alias: "reclaim-trigger-" + uuid.NewString()[:8], Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(trigger).Error)
	job := &models.Job{ID: uuid.New(), Alias: "reclaim-job-" + uuid.NewString()[:8], TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(job).Error)
	atomModel := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(atomModel).Error)
	jobRun := &models.JobRun{ID: uuid.New(), JobID: job.ID, TriggerID: trigger.ID, Status: string(run.StatusRunning), StartedAt: now, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(jobRun).Error)

	lapsed := now.Add(-time.Minute)
	ids := make([]uuid.UUID, 0, n)
	for i := range n {
		task := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atomModel.ID, Name: fmt.Sprintf("step-%d", i), Position: i, CreatedAt: now, UpdatedAt: now}
		require.NoError(t, db.Create(task).Error)
		row := &models.TaskRun{
			ID: uuid.New(), JobRunID: jobRun.ID, TaskID: task.ID, AtomID: atomModel.ID,
			Engine: atomModel.Engine, Image: atomModel.Image, Command: atomModel.Command,
			Status: string(run.TaskStatusRunning), ClaimedBy: claimedBy, ClaimExpiresAt: &lapsed,
			ClaimAttempt: 1, Attempt: 1, MaxAttempts: 1, CreatedAt: now, UpdatedAt: now,
		}
		require.NoError(t, db.Create(row).Error)
		ids = append(ids, row.ID)
	}
	return jobRun.ID, ids
}

// A worker's renewal or a second node's reclaim can commit between this
// reclaim's read and its guarded UPDATE on PostgreSQL READ COMMITTED. The
// UPDATE then skips that row, so the reclaim must publish no lease-expiry or
// task-ready event for it: a task_ready for a row still running wakes every
// worker for nothing, and a lease_expired names an attempt that never lost
// its claim.
func TestPostgresClaimerReclaimExpiredPublishesOnlyTheRowsItReset(t *testing.T) {
	db := openReclaimPostgres(t)
	const node = "postgres-reclaim-worker"
	runID, ids := seedPostgresLapsedRun(t, db, node, 2)
	renewed := ids[0]

	claimer := NewClaimer("node-reaper", run.NewStore(db), time.Minute)
	renewal := make(chan int64, 1)
	started := false
	claimer.afterExpiredRowsRead = func(*gorm.DB) {
		if started {
			return
		}
		started = true
		done := make(chan int64, 1)
		go func() {
			res := db.Model(&models.TaskRun{}).
				Where("id = ? AND claimed_by = ?", renewed, node).
				Update("claim_expires_at", time.Now().UTC().Add(time.Hour))
			if res.Error != nil {
				done <- -1
				return
			}
			done <- res.RowsAffected
		}()
		select {
		case n := <-done:
			renewal <- n
		case <-time.After(2 * time.Second):
			// Blocked by the reclaim's row lock: collect it after the commit.
			go func() { renewal <- <-done }()
		}
	}
	require.NoError(t, claimer.ReclaimExpired(context.Background()))
	require.True(t, started, "the seam must have run")
	require.GreaterOrEqual(t, <-renewal, int64(0), "the renewal must not error")

	var pending int64
	require.NoError(t, db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND status = ?", runID, string(run.TaskStatusPending)).
		Count(&pending).Error)
	for _, typ := range []event.Type{event.TypeTaskLeaseExpired, event.TypeTaskReady} {
		var events int64
		require.NoError(t, db.Model(&models.ExecutionEvent{}).
			Where("run_id = ? AND type = ?", runID, string(typ)).
			Count(&events).Error)
		require.Equal(t, pending, events, "one %s event per row the reclaim re-pended", typ)
	}
}
