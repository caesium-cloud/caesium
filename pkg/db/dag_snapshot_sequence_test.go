package db

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestMigrateDagSnapshotSequenceBackfillsLegacyRows reproduces the upgrade: a
// dag_snapshots table whose rows predate the sequence column (all NULL) is
// numbered per job in (created_at, id) order, a second run changes nothing, and
// one job's numbering never leaks into another's.
func TestMigrateDagSnapshotSequenceBackfillsLegacyRows(t *testing.T) {
	conn := openMigrationTestDB(t)
	require.NoError(t, conn.AutoMigrate(&models.Trigger{}, &models.Job{}, &models.DagSnapshot{}))

	mkJob := func() uuid.UUID {
		trig := models.Trigger{ID: uuid.New(), Type: "cron"}
		require.NoError(t, conn.Create(&trig).Error)
		job := models.Job{ID: uuid.New(), TriggerID: trig.ID, Alias: "j-" + uuid.NewString()[:8]}
		require.NoError(t, conn.Create(&job).Error)
		return job.ID
	}
	base := time.Now().UTC().Truncate(time.Second)
	mkSnap := func(job uuid.UUID, id string, at time.Time) {
		require.NoError(t, conn.Exec(
			"INSERT INTO dag_snapshots (id, job_id, content_hash, git_commit, tasks, edges, created_at) VALUES (?, ?, 'h', '', '[]', '[]', ?)",
			id, job.String(), at).Error)
	}
	jobA, jobB := mkJob(), mkJob()
	// Two rows share created_at: the id breaks the tie. Inserted out of order.
	mkSnap(jobA, "00000000-0000-4000-8000-00000000000b", base.Add(time.Minute))
	mkSnap(jobA, "00000000-0000-4000-8000-00000000000a", base)
	mkSnap(jobA, "00000000-0000-4000-8000-00000000000d", base.Add(time.Minute))
	mkSnap(jobB, "00000000-0000-4000-8000-0000000000b1", base)

	seq := func() map[string]int64 {
		var rows []models.DagSnapshot
		require.NoError(t, conn.Find(&rows).Error)
		out := map[string]int64{}
		for _, r := range rows {
			out[r.ID.String()] = r.Sequence
		}
		return out
	}

	require.NoError(t, MigrateDagSnapshotSequence(conn))
	want := map[string]int64{
		"00000000-0000-4000-8000-00000000000a": 1,
		"00000000-0000-4000-8000-00000000000b": 2,
		"00000000-0000-4000-8000-00000000000d": 3,
		"00000000-0000-4000-8000-0000000000b1": 1,
	}
	require.Equal(t, want, seq())

	require.NoError(t, MigrateDagSnapshotSequence(conn), "idempotent")
	require.Equal(t, want, seq())
}
