package run

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// claimPolicy gives claim-ns two slots, so a promotion can pass the namespace
// gate while the job's own policy still decides.
const claimPolicy = `apiVersion: v1
kind: AccessPolicy
namespaces:
  default: {}
  claim-ns:
    quotas:
      maxConcurrentRuns: 2
`

func reclaimRow(t *testing.T, db *gorm.DB, queued *models.RunQueue) {
	t.Helper()
	require.NoError(t, db.Model(&models.RunQueue{}).Where("id = ?", queued.ID).
		Updates(map[string]any{"claimed_by": "", "claimed_at": nil}).Error)
}

// TestPromotedSkipIsConsumedWithItsOutcome: a promotion the job's own skip
// policy refuses records `skipped` and removes the request in the same
// transaction. The row used to stay (claimed) until the dequeuer deleted it
// separately, so a crash in between let stale-claim recovery promote the
// already-skipped request later and overwrite its outcome with `created`.
// Now nothing is left to recover.
func TestPromotedSkipIsConsumedWithItsOutcome(t *testing.T) {
	db, store := openIdempotencyStore(t)
	useAccessPolicy(t, claimPolicy)
	holder := createNamespacedJob(t, db, "claim-skip-holder", "claim-ns", nil)
	holderRun, err := store.Start(holder.ID, nil)
	require.NoError(t, err)
	job := createNamespacedJob(t, db, "claim-skip", "claim-ns",
		&jobdefschema.Concurrency{MaxRuns: 1, Strategy: jobdefschema.ConcurrencyStrategySkip})
	own, err := store.Start(job.ID, nil)
	require.NoError(t, err)

	parked, err := store.StartWithResult(t.Context(), job.ID, nil, WithStartIdempotencyKey("claim/skip"))
	require.NoError(t, err)
	require.Equal(t, StartOutcomeQueued, parked.Outcome, "claim-ns is full")
	finishRun(t, db, holderRun.ID)

	claimed, err := store.DequeueNextQuotaRun(t.Context(), job.ID, "node-a/skip")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	_, err = store.StartQueuedRun(t.Context(), claimed)
	require.ErrorIs(t, err, ErrRunSkipped, "the job's own run still holds its only slot")

	// The dequeuer "crashes" here: it never runs another statement.
	require.Empty(t, quotaHeldRows(t, db, job.ID), "the skip consumed the request")
	admission, found, err := store.LookupStartAdmission(t.Context(), job.ID, "claim/skip")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, StartOutcomeSkipped, admission.Outcome)

	// Recovery: the reaper and a later drain find nothing to promote.
	finishRun(t, db, own.ID)
	reclaimRow(t, db, claimed)
	again, err := store.DequeueNextRun(t.Context(), job.ID, "node-b/skip")
	require.NoError(t, err)
	require.Nil(t, again)
	require.Equal(t, int64(1), countJobRuns(t, db, job.ID), "the skipped request never executes")
	admission, _, err = store.LookupStartAdmission(t.Context(), job.ID, "claim/skip")
	require.NoError(t, err)
	require.Equal(t, StartOutcomeSkipped, admission.Outcome)
}

// TestStaleDropNeverRemovesAReclaimedRequest: a drainer that decided to drop
// a request and then stalled past the stale threshold must not delete the
// row once another drainer reclaimed it, nor mark the request dropped: the
// current claimant's outcome is the only one.
func TestStaleDropNeverRemovesAReclaimedRequest(t *testing.T) {
	db, store := openIdempotencyStore(t)
	useAccessPolicy(t, claimPolicy)
	holder := createNamespacedJob(t, db, "claim-drop-holder", "claim-ns", nil)
	_, err := store.Start(holder.ID, nil)
	require.NoError(t, err)
	filler := createNamespacedJob(t, db, "claim-drop-filler", "claim-ns", nil)
	fillerRun, err := store.Start(filler.ID, nil)
	require.NoError(t, err)
	job := createNamespacedJob(t, db, "claim-drop", "claim-ns", nil)
	parked, err := store.StartWithResult(t.Context(), job.ID, nil, WithStartIdempotencyKey("claim/drop"))
	require.NoError(t, err)
	require.Equal(t, StartOutcomeQueued, parked.Outcome)
	finishRun(t, db, fillerRun.ID)

	stale, err := store.DequeueNextQuotaRun(t.Context(), job.ID, "node-a/drop")
	require.NoError(t, err)
	require.NotNil(t, stale)
	reclaimRow(t, db, stale)
	current, err := store.DequeueNextQuotaRun(t.Context(), job.ID, "node-b/drop")
	require.NoError(t, err)
	require.NotNil(t, current)

	err = store.DropQueuedRun(t.Context(), stale, QueueDropReasonNamespaceMoved)
	require.ErrorIs(t, err, ErrQueuedRunClaimLost)
	require.Len(t, quotaHeldRows(t, db, job.ID), 1, "the stale drop removed nothing")
	var record models.RunStartIdempotency
	require.NoError(t, db.Where("job_id = ?", job.ID).Take(&record).Error)
	require.Equal(t, string(StartOutcomeQueued), record.Outcome, "and resolved nothing")
	// DeleteQueuedRun is fenced the same way.
	require.NoError(t, store.DeleteQueuedRun(t.Context(), stale))
	require.Len(t, quotaHeldRows(t, db, job.ID), 1, "a stale delete removes nothing")

	started, err := store.StartQueuedRun(t.Context(), current)
	require.NoError(t, err)
	admission, found, err := store.LookupStartAdmission(t.Context(), job.ID, "claim/drop")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, StartOutcomeCreated, admission.Outcome)
	require.Equal(t, started.ID, *admission.RunID)
	require.Equal(t, int64(1), countJobRuns(t, db, job.ID))
	require.Empty(t, quotaHeldRows(t, db, job.ID))
}
