package db

import (
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// TestMigrateDagSnapshotSequenceRetriesContention pins that a transient
// dqlite contention error (the SQLITE_BUSY a read-first transaction gets when
// another node commits before it upgrades to the writer) does not abort
// startup: the per-job transaction is re-run and the backfill still lands.
func TestMigrateDagSnapshotSequenceRetriesContention(t *testing.T) {
	old := BusyRetryBackoffs
	BusyRetryBackoffs = []time.Duration{0, 0, 0}
	t.Cleanup(func() { BusyRetryBackoffs = old })

	conn := openMigrationTestDB(t)
	require.NoError(t, conn.AutoMigrate(&models.Trigger{}, &models.Job{}, &models.DagSnapshot{}))
	trig := models.Trigger{ID: uuid.New(), Type: "cron"}
	require.NoError(t, conn.Create(&trig).Error)
	job := models.Job{ID: uuid.New(), TriggerID: trig.ID, Alias: "retry-job"}
	require.NoError(t, conn.Create(&job).Error)
	id := uuid.New()
	require.NoError(t, conn.Exec(
		"INSERT INTO dag_snapshots (id, job_id, content_hash, git_commit, tasks, edges, created_at) VALUES (?, ?, 'h', '', '[]', '[]', ?)",
		id.String(), job.ID.String(), time.Now().UTC()).Error)

	failures := 0
	require.NoError(t, conn.Callback().Update().Before("gorm:update").Register("test:busy_once", func(tx *gorm.DB) {
		if failures == 0 {
			failures++
			_ = tx.AddError(errors.New("database is locked"))
		}
	}))

	require.NoError(t, MigrateDagSnapshotSequence(conn))
	require.Equal(t, 1, failures, "the contention error must have been injected")
	var row models.DagSnapshot
	require.NoError(t, conn.First(&row, "id = ?", id.String()).Error)
	require.EqualValues(t, 1, row.Sequence)
}
