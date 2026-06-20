package jobdef

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// seedJob inserts a minimal Job (with a required Trigger) and returns the job ID.
func seedJob(t *testing.T, db *gorm.DB) uuid.UUID {
	t.Helper()

	trigID := uuid.New()
	require.NoError(t, db.Create(&models.Trigger{
		ID:   trigID,
		Type: "cron",
	}).Error)

	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{
		ID:        jobID,
		TriggerID: trigID,
		Alias:     "test-job-" + jobID.String()[:8],
	}).Error)

	return jobID
}

// seedSnapshot inserts a DagSnapshot row and returns it.
func seedSnapshot(t *testing.T, db *gorm.DB, jobID uuid.UUID, hash, commit string, at time.Time) *models.DagSnapshot {
	t.Helper()

	snap := &models.DagSnapshot{
		ID:          uuid.New(),
		JobID:       jobID,
		ContentHash: hash,
		GitCommit:   commit,
		Tasks:       datatypes.JSON([]byte(`[]`)),
		Edges:       datatypes.JSON([]byte(`[]`)),
		CreatedAt:   at,
	}
	require.NoError(t, db.Create(snap).Error)
	return snap
}

func TestSnapshotQuery_Latest(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	jobID := seedJob(t, db)
	q := NewSnapshotQuery(context.Background(), db)

	// No snapshot yet → ErrRecordNotFound.
	_, err := q.Latest(jobID)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)

	now := time.Now().UTC()
	older := seedSnapshot(t, db, jobID, "hash-a", "commit-a", now.Add(-time.Hour))
	newer := seedSnapshot(t, db, jobID, "hash-b", "commit-b", now)

	snap, err := q.Latest(jobID)
	require.NoError(t, err)
	require.Equal(t, newer.ID, snap.ID, "expected the newer snapshot")
	_ = older // suppress unused warning
}

func TestSnapshotQuery_ByContentHash(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	jobID := seedJob(t, db)
	q := NewSnapshotQuery(context.Background(), db)

	now := time.Now().UTC()
	seedSnapshot(t, db, jobID, "hash-x", "commit-x", now)

	// Matching hash.
	snap, err := q.ByContentHash(jobID, "hash-x")
	require.NoError(t, err)
	require.Equal(t, "hash-x", snap.ContentHash)

	// Non-existent hash.
	_, err = q.ByContentHash(jobID, "does-not-exist")
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestSnapshotQuery_ByGitCommit(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	jobID := seedJob(t, db)
	q := NewSnapshotQuery(context.Background(), db)

	// Both the query layer and the service layer reject an empty commit.
	_, err := q.ByGitCommit(jobID, "")
	require.Error(t, err)

	now := time.Now().UTC()
	seedSnapshot(t, db, jobID, "hash-y", "abc123", now)

	snap, err := q.ByGitCommit(jobID, "abc123")
	require.NoError(t, err)
	require.Equal(t, "abc123", snap.GitCommit)

	_, err = q.ByGitCommit(jobID, "not-a-commit")
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestSnapshotQuery_List(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	jobID := seedJob(t, db)
	q := NewSnapshotQuery(context.Background(), db)

	// No rows yet → empty slice, no error.
	snaps, err := q.List(jobID)
	require.NoError(t, err)
	require.Empty(t, snaps)

	now := time.Now().UTC()
	seedSnapshot(t, db, jobID, "hash-1", "", now.Add(-2*time.Hour))
	seedSnapshot(t, db, jobID, "hash-2", "", now.Add(-time.Hour))
	seedSnapshot(t, db, jobID, "hash-3", "", now)

	snaps, err = q.List(jobID)
	require.NoError(t, err)
	require.Len(t, snaps, 3)
	// Expect newest first.
	require.Equal(t, "hash-3", snaps[0].ContentHash)
	require.Equal(t, "hash-2", snaps[1].ContentHash)
	require.Equal(t, "hash-1", snaps[2].ContentHash)
}

func TestSnapshotQuery_IsolatedByJob(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	jobA := seedJob(t, db)
	jobB := seedJob(t, db)
	q := NewSnapshotQuery(context.Background(), db)

	now := time.Now().UTC()
	seedSnapshot(t, db, jobA, "hash-a", "", now)
	seedSnapshot(t, db, jobB, "hash-b", "", now)

	snapA, err := q.Latest(jobA)
	require.NoError(t, err)
	require.Equal(t, "hash-a", snapA.ContentHash)

	snapB, err := q.Latest(jobB)
	require.NoError(t, err)
	require.Equal(t, "hash-b", snapB.ContentHash)
}
