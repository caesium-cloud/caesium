package job

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/authz"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestQueueCarriesHeldReasonAndKeepsItsOwnersRows pins D2 and the queue
// ownership rule: a quota-held row reports held_reason (omitted otherwise);
// a confined reader sees only rows recorded in a namespace it may read,
// renumbered, never a legacy row with no recorded namespace; an unrestricted
// reader sees every row exactly as before.
func TestQueueCarriesHeldReasonAndKeepsItsOwnersRows(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
	svc := &jobService{ctx: context.Background(), db: db}
	job, err := svc.Create(&CreateRequest{TriggerID: uuid.New(), Alias: "queue-held-reason"})
	require.NoError(t, err)

	now := time.Now().UTC()
	marketing, finance := "marketing", "finance"
	quota := models.RunQueueHeldReasonNamespaceQuota
	oldOwner, held, legacy, plain := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, row := range []models.RunQueue{
		{ID: oldOwner, JobID: job.ID, Priority: 2, CreatedAt: now.Add(-4 * time.Minute), Namespace: &marketing, HeldReason: &quota},
		{ID: held, JobID: job.ID, Priority: 2, CreatedAt: now.Add(-3 * time.Minute), Namespace: &finance, HeldReason: &quota},
		{ID: legacy, JobID: job.ID, Priority: 2, CreatedAt: now.Add(-2 * time.Minute)},
		{ID: plain, JobID: job.ID, Priority: 2, CreatedAt: now.Add(-time.Minute), Namespace: &finance},
	} {
		require.NoError(t, db.Create(&row).Error)
	}

	all, err := svc.Queue(job.ID)
	require.NoError(t, err)
	require.Len(t, all, 4, "an unrestricted reader sees every row")
	require.Equal(t, models.RunQueueHeldReasonNamespaceQuota, all[1].HeldReason)
	require.Empty(t, all[3].HeldReason)
	encoded, err := json.Marshal(all[3])
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "held_reason", "held_reason is omitted on a row without one")

	confined := &jobService{ctx: authz.WithAllowedNamespaces(context.Background(), []string{finance}), db: db}
	visible, err := confined.Queue(job.ID)
	require.NoError(t, err)
	require.Len(t, visible, 2, "a finance reader sees only finance's rows: not marketing's, not the legacy row")
	require.Equal(t, held, visible[0].ID)
	require.Equal(t, 1, visible[0].Position, "positions are renumbered among the visible rows")
	require.Equal(t, plain, visible[1].ID)
	require.Equal(t, 2, visible[1].Position)

	// CancelQueuedRun hands the check the row's recorded namespace and deletes
	// nothing when it refuses; without a check it cancels as before.
	refused := errors.New("refused")
	var seen []*string
	checked := &jobService{ctx: WithQueuedRunCancelCheck(context.Background(), func(namespace *string) error {
		seen = append(seen, namespace)
		if namespace == nil || *namespace != finance {
			return refused
		}
		return nil
	}), db: db}
	require.ErrorIs(t, checked.CancelQueuedRun(job.ID, oldOwner), refused)
	require.ErrorIs(t, checked.CancelQueuedRun(job.ID, legacy), refused)
	require.ErrorIs(t, checked.CancelQueuedRun(job.ID, uuid.New()), runstorage.ErrQueuedRunNotFound,
		"a missing row is not found before any check")
	require.NoError(t, checked.CancelQueuedRun(job.ID, plain))
	require.Len(t, seen, 3)
	require.Equal(t, marketing, *seen[0])
	require.Nil(t, seen[1], "a pre-upgrade row records no namespace")
	require.Equal(t, finance, *seen[2])
	var remaining int64
	require.NoError(t, db.Model(&models.RunQueue{}).Where("job_id = ?", job.ID).Count(&remaining).Error)
	require.Equal(t, int64(3), remaining)
	require.NoError(t, svc.CancelQueuedRun(job.ID, legacy), "without a check the cancel is unchanged")
}
