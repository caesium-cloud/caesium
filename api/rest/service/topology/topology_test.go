package topology

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

func seedJob(t *testing.T, db *gorm.DB) uuid.UUID {
	t.Helper()

	trigID := uuid.New()
	require.NoError(t, db.Create(&models.Trigger{ID: trigID, Type: "cron"}).Error)

	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{
		ID:        jobID,
		TriggerID: trigID,
		Alias:     "svc-test-" + jobID.String()[:8],
	}).Error)

	return jobID
}

func seedSnap(t *testing.T, db *gorm.DB, jobID uuid.UUID, hash, commit string, at time.Time) *models.DagSnapshot {
	t.Helper()

	snap := &models.DagSnapshot{
		ID:          uuid.New(),
		JobID:       jobID,
		ContentHash: hash,
		GitCommit:   commit,
		Tasks:       datatypes.JSON([]byte(`[{"name":"step1","image":"busybox:1.36.1"}]`)),
		Edges:       datatypes.JSON([]byte(`[]`)),
		CreatedAt:   at,
	}
	require.NoError(t, db.Create(snap).Error)
	return snap
}

func TestService_Latest(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	jobID := seedJob(t, db)
	svc := ServiceWithDB(context.Background(), db)

	_, err := svc.Latest(jobID)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)

	now := time.Now().UTC()
	seedSnap(t, db, jobID, "h1", "", now.Add(-time.Hour))
	seedSnap(t, db, jobID, "h2", "", now)

	snap, err := svc.Latest(jobID)
	require.NoError(t, err)
	require.Equal(t, "h2", snap.ContentHash)
}

func TestService_ByContentHash(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	jobID := seedJob(t, db)
	svc := ServiceWithDB(context.Background(), db)

	// Empty hash returns error.
	_, err := svc.ByContentHash(jobID, "")
	require.Error(t, err)

	now := time.Now().UTC()
	seedSnap(t, db, jobID, "abc", "", now)

	snap, err := svc.ByContentHash(jobID, "abc")
	require.NoError(t, err)
	require.Equal(t, "abc", snap.ContentHash)
}

func TestService_ByGitCommit(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	jobID := seedJob(t, db)
	svc := ServiceWithDB(context.Background(), db)

	now := time.Now().UTC()
	seedSnap(t, db, jobID, "hh", "deadbeef", now)

	snap, err := svc.ByGitCommit(jobID, "deadbeef")
	require.NoError(t, err)
	require.Equal(t, "deadbeef", snap.GitCommit)

	_, err = svc.ByGitCommit(jobID, "missing")
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestService_List(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	jobID := seedJob(t, db)
	svc := ServiceWithDB(context.Background(), db)

	snaps, err := svc.List(jobID)
	require.NoError(t, err)
	require.Empty(t, snaps)

	now := time.Now().UTC()
	seedSnap(t, db, jobID, "first", "", now.Add(-time.Hour))
	seedSnap(t, db, jobID, "second", "", now)

	snaps, err = svc.List(jobID)
	require.NoError(t, err)
	require.Len(t, snaps, 2)
	require.Equal(t, "second", snaps[0].ContentHash)
	require.Equal(t, "first", snaps[1].ContentHash)
}
