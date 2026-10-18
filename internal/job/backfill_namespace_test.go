package job

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/runlife"
	"github.com/stretchr/testify/require"
)

// TestBackfillStopsWhenJobLeavesItsNamespace: every backfill child is admitted
// only while the job is still in the backfill's persisted namespace (checked
// inside the admission transaction). A job moved between two dates gets no
// further child under its new owner, and the backfill finishes as failed
// (identity-and-access B4).
func TestBackfillStopsWhenJobLeavesItsNamespace(t *testing.T) {
	b, j, bStore, rStore := backfillLifetimeFixture(t)
	db := rStore.DB()
	var executed atomic.Int32
	owner := runlife.New(context.Background())
	runBackfill(runlife.WithSupervisor(t.Context(), owner), b, j, mustParseCron(t, "0 * * * *"), time.UTC, bStore, rStore,
		func(context.Context, *models.Job, map[string]string) error {
			// The first date ran; the job now moves to finance.
			if executed.Add(1) == 1 {
				require.NoError(t, db.Model(&models.Job{}).Where("id = ?", j.ID).Update("namespace", "finance").Error)
			}
			return nil
		})
	owner.CloseAndCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, owner.Wait(ctx))

	require.EqualValues(t, 1, executed.Load(), "no date runs after the move")
	var children []models.JobRun
	require.NoError(t, db.Where("backfill_id = ?", b.ID).Find(&children).Error)
	require.Len(t, children, 1)
	require.Equal(t, models.DefaultNamespace, children[0].Namespace)
	stored, err := bStore.Get(b.ID)
	require.NoError(t, err)
	require.Equal(t, models.BackfillStatusFailed, stored.Status, "a backfill cut short by a move is a failure, not a success")
}
