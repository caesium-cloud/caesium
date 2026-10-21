package run

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// pgClaimFixture is one policy-free job in namespace ns on PostgreSQL, with
// the admission tables migrated. Opt-in via CAESIUM_TEST_POSTGRES_DSN.
func pgClaimFixture(t *testing.T, ns, policyBody string) (*gorm.DB, *Store, uuid.UUID) {
	t.Helper()
	db := openDeadlinePostgres(t)
	require.NoError(t, db.AutoMigrate(&models.RunQueue{}, &models.RunStartIdempotency{}, &models.DatasetDeclaration{}, &models.DatasetHold{}))
	useAccessPolicy(t, policyBody)
	now := time.Now().UTC()
	triggerID := uuid.New()
	require.NoError(t, db.Create(&models.Trigger{
		ID: triggerID, Alias: "pg-claim-trigger-" + ns, Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now,
	}).Error)
	job := &models.Job{ID: uuid.New(), TriggerID: triggerID, Alias: "pg-claim-" + ns, Namespace: ns}
	require.NoError(t, db.Create(job).Error)
	return db, NewStore(db), job.ID
}

// pgSeedHeldRequest parks one quota-held request for jobID in ns.
func pgSeedHeldRequest(t *testing.T, db *gorm.DB, jobID uuid.UUID, ns string) {
	t.Helper()
	reason := models.RunQueueHeldReasonNamespaceQuota
	require.NoError(t, db.Create(&models.RunQueue{
		ID: uuid.New(), JobID: jobID, Priority: PriorityNormalValue, CreatedAt: time.Now().UTC(),
		Namespace: &ns, HeldReason: &reason,
	}).Error)
}

// pgReclaim releases a row as the stale-claim reaper does.
func pgReclaim(db *gorm.DB, queueID uuid.UUID) error {
	return db.Model(&models.RunQueue{}).Where("id = ?", queueID).
		Updates(map[string]any{"claimed_by": "", "claimed_at": nil}).Error
}

// TestPostgresStaleClaimantRacingThePromoterYieldsOneRun: on PostgreSQL every
// node drains. Node A claimed a request and paused past the stale threshold;
// node B's reaper released it and B claimed it. Both promote at once into a
// namespace with two free slots (the advisory lock only serializes them):
// exactly one run is created per request.
func TestPostgresStaleClaimantRacingThePromoterYieldsOneRun(t *testing.T) {
	db, store, jobID := pgClaimFixture(t, "quota-a", `apiVersion: v1
kind: AccessPolicy
namespaces:
  default: {}
  quota-a:
    quotas:
      maxConcurrentRuns: 2
`)
	for round := range 5 {
		pgSeedHeldRequest(t, db, jobID, "quota-a")
		stale, err := store.DequeueNextRun(t.Context(), jobID, fmt.Sprintf("node-a/%d", round))
		require.NoError(t, err)
		require.NotNil(t, stale)
		require.NoError(t, pgReclaim(db, stale.ID))
		current, err := store.DequeueNextRun(t.Context(), jobID, fmt.Sprintf("node-b/%d", round))
		require.NoError(t, err)
		require.NotNil(t, current)
		require.Equal(t, stale.ID, current.ID)

		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for i, queued := range []*models.RunQueue{stale, current} {
			wg.Go(func() {
				<-start
				_, errs[i] = store.StartQueuedRun(t.Context(), queued)
			})
		}
		close(start)
		wg.Wait()
		require.ErrorIs(t, errs[0], ErrQueuedRunClaimLost, "round %d: the stale claimant creates nothing", round)
		require.NoError(t, errs[1], "round %d", round)
		var runs int64
		require.NoError(t, db.Model(&models.JobRun{}).Where("job_id = ? AND status = ?", jobID, string(StatusRunning)).Count(&runs).Error)
		require.Equal(t, int64(1), runs, "round %d: one request, one run", round)
		var left int64
		require.NoError(t, db.Model(&models.RunQueue{}).Where("job_id = ?", jobID).Count(&left).Error)
		require.Zero(t, left, "round %d: the request is consumed with its run", round)
		require.NoError(t, db.Model(&models.JobRun{}).Where("job_id = ?", jobID).Update("status", string(StatusSucceeded)).Error)
	}
}

type stealClaimKey struct{}

// TestPostgresClaimLostMidAdmissionRollsBackTheRun is the interleaving the
// fail-fast check cannot see: node A's admission has passed its claim check
// when node B reclaims, re-claims and promotes the same request. A's
// transaction then finds the row no longer carries A's claim and rolls back
// the run it inserted: exactly one run.
func TestPostgresClaimLostMidAdmissionRollsBackTheRun(t *testing.T) {
	// No quota here: the namespace advisory lock A would hold would make B
	// wait for A, and this test needs B to commit inside A's transaction.
	db, store, jobID := pgClaimFixture(t, "claims", `apiVersion: v1
kind: AccessPolicy
namespaces:
  default: {}
  claims: {}
`)
	pgSeedHeldRequest(t, db, jobID, "claims")
	stale, err := store.DequeueNextRun(t.Context(), jobID, "node-a/mid")
	require.NoError(t, err)
	require.NotNil(t, stale)

	var (
		once      sync.Once
		stolenErr error
		winner    *JobRun
	)
	const hook = "test:steal-claim-mid-admission"
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Context.Value(stealClaimKey{}) == nil {
			return
		}
		if _, ok := tx.Statement.Model.(*models.JobRun); !ok {
			return
		}
		once.Do(func() {
			if err := pgReclaim(db, stale.ID); err != nil {
				stolenErr = err
				return
			}
			current, err := store.DequeueNextRun(context.Background(), jobID, "node-b/mid")
			if err != nil || current == nil {
				stolenErr = fmt.Errorf("node B claim: %v (row %v)", err, current)
				return
			}
			winner, stolenErr = store.StartQueuedRun(context.Background(), current)
		})
	}))
	t.Cleanup(func() { _ = db.Callback().Create().Remove(hook) })

	ctx := context.WithValue(t.Context(), stealClaimKey{}, true)
	_, err = store.StartQueuedRun(ctx, stale)
	require.NoError(t, stolenErr, "node B promotes inside A's admission")
	require.NotNil(t, winner)
	require.ErrorIs(t, err, ErrQueuedRunClaimLost)
	var runs []models.JobRun
	require.NoError(t, db.Where("job_id = ?", jobID).Find(&runs).Error)
	require.Len(t, runs, 1, "one request, one run")
	require.Equal(t, winner.ID, runs[0].ID)
	var left int64
	require.NoError(t, db.Model(&models.RunQueue{}).Where("job_id = ?", jobID).Count(&left).Error)
	require.Zero(t, left)
}
