package agent

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// An incident outlives its job: the history of a soft-deleted job stays readable
// by alias as it is with no alias at all, and a live job since given that alias
// does not take the read over.
func TestHistoryByOwnAliasAfterTheJobIsSoftDeleted(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	now := time.Now().UTC()

	jobID := seedJob(t, db, "deleted-job")
	inc := seedIncident(t, db, jobID)
	own := uuid.New()
	require.NoError(t, db.Create(&models.JobRun{ID: own, JobID: jobID, Status: "failed", StartedAt: now, CreatedAt: now, UpdatedAt: now}).Error)
	require.NoError(t, db.Where("id = ?", jobID).Delete(&models.Job{}).Error)

	svc := &Service{ctx: context.Background(), db: db}
	runs, err := svc.History(inc, "deleted-job", nil)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, own, runs[0].ID)

	// A foreign alias is still refused, deleted job or not.
	_, err = svc.History(inc, "someone-else", nil)
	require.ErrorIs(t, err, ErrForbiddenJob)
}

func TestMetricSeriesNeedsAHoldIncident(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	inc := seedIncident(t, db, seedJob(t, db, "plain-job"))

	svc := &Service{ctx: context.Background(), db: db}
	_, err := svc.MetricSeries(inc, "rowCount")
	require.Error(t, err)
}
